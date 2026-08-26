import type { RoleOverrides, RoleTemplate } from '@/lib/api'

function sameArgs(a: string[], b: string[]) {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

/**
 * Converts an edited effective role back into its sparse inheritance layer.
 * Equal values become null (keep following the source); an empty args array is
 * retained when the source has arguments because [] means explicitly none.
 */
export function roleOverrides(value: RoleTemplate, inherited: RoleTemplate): RoleOverrides {
  return {
    harnessOverride: value.harness === inherited.harness ? null : value.harness,
    modelOverride: value.model === inherited.model ? null : value.model,
    argsOverride: sameArgs(value.args, inherited.args) ? null : [...value.args],
    receiveOverride: value.receive === inherited.receive ? null : value.receive,
    batchMaxItemsOverride:
      value.batchMaxItems === inherited.batchMaxItems ? null : value.batchMaxItems,
    batchMaxAgeSecOverride:
      value.batchMaxAgeSec === inherited.batchMaxAgeSec ? null : value.batchMaxAgeSec,
    promptOverride: value.prompt === inherited.prompt ? null : value.prompt,
    gateOverride: value.gate === inherited.gate ? null : value.gate,
  }
}
