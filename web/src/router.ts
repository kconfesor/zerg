/**
 * One route per view, under the project it belongs to.
 *
 * The project is in the path — /p/<id>/board — because every view is scoped to
 * one, and a URL that does not say which is a URL that cannot be reloaded,
 * bookmarked or sent to anyone. Before this, reloading landed on whichever
 * project the daemon happened to list first, which is not a thing the reader
 * asked for.
 *
 * History mode rather than hashes: the daemon serves index.html for any path it
 * does not recognise, so deep links have always worked.
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

// Attention is deliberately not among these. What is waiting on a person
// interrupts whatever you are reading; sending you to another page to see it,
// and back again afterwards, loses your place both ways.
export const VIEWS = [
  'board',
  'projects',
  'activity',
  'spend',
  'chat',
  'history',
  'team',
  'settings',
] as const

export type View = (typeof VIEWS)[number]

/** The path for a view within a project. */
export function viewPath(projectId: string | null | undefined, view: View): string {
  return projectId ? `/p/${projectId}/${view}` : `/${view}`
}

// The shell renders every view itself, so a route only has to name one.
const blank = { render: () => null }

const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/board' },

  // Project-scoped, which is the shape every link should have.
  ...VIEWS.map((name) => ({
    path: `/p/:projectId/${name}`,
    name,
    component: blank,
  })),

  // Bare views stay valid so an old link still opens something, and the shell
  // rewrites them to the current project once it knows one.
  ...VIEWS.map((name) => ({
    path: `/${name}`,
    name: `bare-${name}`,
    component: blank,
  })),

  // Readiness stopped being a screen and became a tab in Settings, since it is
  // a setup step rather than something you watch. Old links still land
  // somewhere sensible instead of on the board with no explanation.
  { path: '/p/:projectId/readiness', redirect: (to) => `/p/${to.params.projectId}/settings` },
  { path: '/readiness', redirect: '/settings' },

  // An unknown path is a typo or a stale link, and the board is the honest
  // place to land rather than a 404 page for an app with seven screens.
  { path: '/:pathMatch(.*)*', redirect: '/board' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

/** The view a route names, whichever of the two shapes it is. */
export function viewOf(name: unknown): View {
  const n = String(name ?? '').replace(/^bare-/, '')
  return (VIEWS as readonly string[]).includes(n) ? (n as View) : 'board'
}
