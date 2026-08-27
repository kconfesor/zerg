import { afterEach, describe, expect, it } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import RoleOverrideDialog from './RoleOverrideDialog.vue'

// The dialog teleports to the body, and a wrapper nobody unmounts leaves it
// there for the next test to read.
enableAutoUnmount(afterEach)
import { roleOverrides } from '@/lib/role-overrides'
import type { RoleTemplate } from '@/lib/api'

const inherited: RoleTemplate = {
  id: 'coder',
  name: 'coder',
  harness: 'claude',
  model: 'sonnet',
  args: ['--preset'],
  thinking: '',
  receive: 'task',
  batchMaxItems: 8,
  batchMaxAgeSec: 300,
  prompt: 'default prompt',
  gate: 'none',
  builtin: true,
}

describe('role override editing', () => {
  it('keeps explicit empty arguments distinct from inheritance', () => {
    expect(roleOverrides({ ...inherited, args: [] }, inherited)).toMatchObject({
      harnessOverride: null,
      modelOverride: null,
      argsOverride: [],
      promptOverride: null,
    })
  })

  it('turns values reset to their defaults back into live inheritance', () => {
    expect(roleOverrides({ ...inherited, args: [...inherited.args] }, inherited)).toEqual({
      harnessOverride: null,
      modelOverride: null,
      argsOverride: null,
      thinkingOverride: null,
      receiveOverride: null,
      batchMaxItemsOverride: null,
      batchMaxAgeSecOverride: null,
      promptOverride: null,
      gateOverride: null,
    })
  })

  it('preserves an explicitly cleared string field', () => {
    expect(roleOverrides({ ...inherited, prompt: '' }, inherited).promptOverride).toBe('')
  })
})

describe('thinking level', () => {
  const dialog = (thinking: Record<string, string[]>) =>
    mount(RoleOverrideDialog, {
      props: {
        open: true,
        role: { ...inherited, thinking: 'high' },
        inherited,
        scope: 'Team role settings',
        harnesses: ['claude', 'nothink'],
        models: {},
        thinking,
      },
    })

  it('offers the levels the harness accepts, and says which is inherited', async () => {
    // The dialog teleports to the body, so it is read there, and only after
    // the teleport has happened.
    const w = dialog({ claude: ['low', 'medium', 'high', 'xhigh', 'max'] })
    await w.vm.$nextTick()
    expect(document.body.textContent).toContain('Thinking')
    // Inherited is nothing, which is the harness's own setting rather than a
    // level zerg picked.
    expect(document.body.textContent).toContain('harness default')
  })

  it('leaves the field out for a harness that has no such control', async () => {
    // The levels come from the adapter, so a harness reporting none has no
    // field rather than a picker of options it would refuse.
    const w = dialog({ claude: [] })
    await w.vm.$nextTick()
    expect(document.body.textContent).not.toContain('Thinking')
  })
})
