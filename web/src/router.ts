/**
 * One route per view.
 *
 * The views were a ref, so the address bar never moved: a link to Settings did
 * not exist, back did nothing, and a reload put you on the board wherever you
 * had been. On a phone, where reloading is how you recover from anything, that
 * loses your place every time.
 *
 * History mode rather than hashes, because the daemon already serves index.html
 * for any path it does not recognise — deep links were served correctly long
 * before there were any to serve.
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

/** The view names, which double as the path segments. */
export const VIEWS = [
  'board',
  'projects',
  'activity',
  'chat',
  'team',
  'attention',
  'readiness',
  'settings',
] as const

export type View = (typeof VIEWS)[number]

const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/board' },
  ...VIEWS.map((name) => ({
    path: `/${name}`,
    name,
    // The shell renders every view itself, so a route only has to name one.
    // Splitting them into components would move markup without changing
    // anything about what is on screen.
    component: { render: () => null },
  })),
  // An unknown path is a typo or a stale link, and the board is the honest
  // place to land — not a 404 page for an app with seven screens.
  { path: '/:pathMatch(.*)*', redirect: '/board' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})
