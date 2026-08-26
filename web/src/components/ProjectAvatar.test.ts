import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ProjectAvatar from './ProjectAvatar.vue'
import type { Project } from '@/lib/api'

function project(over: Partial<Project> = {}): Project {
  return {
    id: '01M0VPTPS4TY4MC5MJPH0S4MJ2',
    path: '/src/calc2',
    name: 'calc2',
    baseBranch: 'main',
    integration: 'merge',
    createdAt: '2026-01-01T00:00:00Z',
    ...over,
    prDraft: over.prDraft ?? false,
  }
}

describe('ProjectAvatar', () => {
  it("shows the repository's own mark when one is picked", () => {
    const w = mount(ProjectAvatar, { props: { project: project({ icon: 'public/logo.svg' }) } })
    const img = w.get('img')
    expect(img.attributes('src')).toContain('/api/projects/01M0VPTPS4TY4MC5MJPH0S4MJ2/icon')
    expect(img.attributes('src')).toContain(encodeURIComponent('public/logo.svg'))
  })

  it('falls back to initials when the mark has been deleted from the repository', async () => {
    // A path that no longer resolves 404s, and a project that looks like it has
    // no mark is exactly what the initials are for. Nothing is reported: the
    // file is the repository's business, not the cockpit's.
    const w = mount(ProjectAvatar, { props: { project: project({ icon: 'public/gone.png' }) } })
    await w.get('img').trigger('error')
    expect(w.find('img').exists()).toBe(false)
    expect(w.text()).toBe('CA')
  })

  it('falls back to initials from the name', () => {
    expect(mount(ProjectAvatar, { props: { project: project() } }).text()).toBe('CA')
    expect(
      mount(ProjectAvatar, { props: { project: project({ name: 'credix-platform' }) } }).text(),
    ).toBe('CP')
    expect(
      mount(ProjectAvatar, { props: { project: project({ name: 'my new thing' }) } }).text(),
    ).toBe('MN')
  })

  it('keeps its colour when the project is renamed', () => {
    // The colour is the thing you recognise, so it has to survive an edit to
    // the thing you are most likely to change. Derived from the id, not the
    // name, for exactly that reason.
    const before = mount(ProjectAvatar, { props: { project: project() } })
    const after = mount(ProjectAvatar, { props: { project: project({ name: 'renamed' }) } })
    expect(after.attributes('style')).toBe(before.attributes('style'))
  })

  it('gives different projects different colours', () => {
    const a = mount(ProjectAvatar, { props: { project: project({ id: 'aaaa' }) } })
    const b = mount(ProjectAvatar, { props: { project: project({ id: 'aaab' }) } })
    expect(a.attributes('style')).not.toBe(b.attributes('style'))
  })

  it('survives having no project at all', () => {
    expect(mount(ProjectAvatar, { props: { project: null } }).text()).toBe('?')
  })

  it('is hidden from screen readers, since the name is always beside it', () => {
    const w = mount(ProjectAvatar, { props: { project: project() } })
    expect(w.attributes('aria-hidden')).toBe('true')
  })
})
