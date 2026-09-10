import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import FeaturePanel from './FeaturePanel.vue'
import { api, type FeatureDetail } from '@/lib/api'

vi.mock('@/lib/api', () => ({ api: {
  featureDetail: vi.fn(), featureFiles: vi.fn(), featureFile: vi.fn(),
  rejectFeature: vi.fn(), refreshFeature: vi.fn(), landFeature: vi.fn(),
  retryCard: vi.fn(), waiveDependency: vi.fn(), cancelFeature: vi.fn(), approvePlan: vi.fn(), rejectPlan: vi.fn(),
} }))
enableAutoUnmount(afterEach)

let data: FeatureDetail
beforeEach(() => {
  vi.resetAllMocks()
  const task = { id: 'f', projectId: 'p', kind: 'feature' as const, name: 'Coherent delivery', body: 'Original acceptance requirement', lane: '', state: 'working' as const, createdAt: '2026-07-01', activeMs: 0, reworkCount: 0, tokens: 600, costUsd: 5 }
  data = {
    task, integration: 'merge', baseBranch: 'main', history: [],
    run: { featureId: 'f', branch: 'zerg-feature-f', baseSha: 'base', headSha: 'reviewed-head', state: 'running' },
    plans: [{ id: 'plan', featureId: 'f', n: 1, digest: 'digest', state: 'approved', itemCount: 1, estimateTokens: 0, estimateCostUsd: 0, proseSha: 'plan-proof', createdAt: task.createdAt,
      items: [{ id: 'item', position: 0, name: 'Implementation', body: 'Every accepted requirement', priority: 70, childTaskId: 'child' }] }],
    reviews: [{ id: 'review', featureId: 'f', headSha: 'reviewed-head', verdict: 'ok', decidedBy: 'supervisor', note: 'Acceptance checks passed', createdAt: task.createdAt }],
    children: [{ ...task, id: 'child', kind: 'work', parentId: 'f', name: 'Implementation', state: 'done' }],
    usage: { key: 'f', turns: 2, inputTokens: 100, outputTokens: 100, cacheReadTokens: 400, cacheWriteTokens: 0, costUsd: 5, unpricedTurns: 0, subscriptionTurns: 0 },
  }
  vi.mocked(api.featureDetail).mockImplementation(async () => structuredClone(data))
  vi.mocked(api.featureFiles).mockImplementation(async (_id, head, evidence) => ({ base: 'base', head,
    files: evidence ? [{ path: 'plan.md', status: 'A', added: 1, removed: 0, content: '# Committed rationale\n<script>bad()</script>' }] : [
      { path: 'first.go', status: 'A', added: 1, removed: 0, diff: '@@ -0,0 +1 @@\n+first requirement\n' },
      { path: 'later.go', status: 'A', added: 1, removed: 0, deferred: true },
    ],
  }))
  vi.mocked(api.featureFile).mockResolvedValue({ path: 'later.go', status: 'A', added: 1, removed: 0, diff: '@@ -0,0 +1 @@\n+later requirement\n' })
})

async function open() {
  const w = mount(FeaturePanel, { props: { featureId: 'f' } })
  await flushPromises()
  return w
}
function button(w: Awaited<ReturnType<typeof open>>, label: string) {
  const b = w.findAll('button').find(b => b.text().includes(label))
  if (!b) throw new Error(`Missing button: ${label}`)
  return b
}

describe('a feature as the unit a person reviews', () => {
  it('presents original scope, committed evidence, whole diff, deferred files and aggregate cost', async () => {
    const w = await open()
    expect(w.text()).toContain('Original acceptance requirement')
    expect(w.text()).toContain('Every accepted requirement')
    expect(w.text()).toContain('600 tokens · $5.00')
    await button(w, 'Read evidence').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Committed rationale')
    expect(w.find('script').exists()).toBe(false)
    await button(w, 'Read whole change').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('first requirement')
    expect(w.text()).not.toContain('later requirement')
    await button(w, 'Read file').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('later requirement')
    expect(w.find('[aria-label="Comment on this line"]').exists()).toBe(false)
  })

  it('sends a displayed head back for correction, instead of cancelling it', async () => {
    vi.mocked(api.rejectFeature).mockImplementation(async (_id, head, note) => {
      if (head !== data.run!.headSha) throw new Error('stale consent')
      data.reviews.push({ id: 'human', featureId: 'f', headSha: head, verdict: 'reject', decidedBy: 'operator', note, createdAt: '2026-07-02' })
    })
    vi.mocked(api.retryCard).mockImplementation(async (id) => {
      data.children.find(c => c.id === id)!.state = 'queued'
    })
    const w = await open()
    expect(button(w, 'Send back').attributes('disabled')).toBeDefined()
    await w.get('textarea').setValue('Missing an acceptance check')
    await button(w, 'Send back').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('operator: reject')
    expect(w.text()).toContain('Missing an acceptance check')
    expect(w.findAll('button').some(b => b.text().includes('Land on'))).toBe(false)
    expect(button(w, 'Retry').exists()).toBe(true)
    expect(w.text()).not.toContain('cancelled')
    await button(w, 'Retry').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('queued')
    expect(w.findAll('button').some(b => b.text().startsWith('Retry'))).toBe(false)
  })

  it('keeps a land failure beside its controls and leaves the feature open', async () => {
    vi.mocked(api.landFeature).mockRejectedValue(new Error('main moved; refresh and review again'))
    const w = await open()
    await button(w, 'Land on main').trigger('click')
    await flushPromises()
    expect(w.get('[role="alert"]').text()).toContain('main moved')
    expect(button(w, 'Refresh from main').exists()).toBe(true)
    expect(button(w, 'Land on main').attributes('disabled')).toBeUndefined()
    expect(data.run?.state).toBe('running')
  })

  it('cannot present a missing planned card as ready to land', async () => {
    data.children = []
    const w = await open()
    expect(w.text()).toContain('Required card is missing')
    expect(w.findAll('button').some(b => b.text().includes('Land on'))).toBe(false)
  })

  it('keeps landed features readable without offering another land', async () => {
    data.run!.state = 'done'
    data.task.state = 'done'
    const w = await open()
    expect(w.text()).toContain('Every accepted requirement')
    expect(w.text()).toContain('Acceptance checks passed')
    expect(button(w, 'Read whole change').exists()).toBe(true)
    expect(w.findAll('button').some(b => /Land on|Cancel feature|Send back|Retry/.test(b.text()))).toBe(false)
  })
})
