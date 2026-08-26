import { describe, expect, it } from 'vitest'
import { roleOverrides } from '@/lib/role-overrides'
import type { RoleTemplate } from '@/lib/api'

const inherited: RoleTemplate = {
  id: 'coder',
  name: 'coder',
  harness: 'claude',
  model: 'sonnet',
  args: ['--preset'],
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
