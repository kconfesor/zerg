import { describe, expect, it } from 'vitest'
import type { TeamPresetRole } from '@/lib/api'

/**
 * The two rules the pipeline column has to keep, at the level they live.
 *
 * `enabled` never stopped being read — the terminal role is the last *enabled*
 * one, and the overmind spawns only enabled roles — but the control that set it
 * was dropped, so a role stored disabled could not be turned back on.
 */

function role(id: string, enabled: boolean, position: number): TeamPresetRole {
  return { templateId: id, position, enabled } as TeamPresetRole
}

/** What setEnabled does to the list it saves. */
function setEnabled(roles: TeamPresetRole[], templateId: string, enabled: boolean) {
  return roles.map((r) => ({ ...r, enabled: r.templateId === templateId ? enabled : r.enabled }))
}

/** Terminality is the last enabled role, which is why parking one moves it. */
function terminal(roles: TeamPresetRole[]): string {
  for (let i = roles.length - 1; i >= 0; i--) if (roles[i].enabled) return roles[i].templateId
  return ''
}

describe('parking a role', () => {
  const team = [role('planner', true, 0), role('coder', true, 1), role('reviewer', true, 2)]

  it('turns one off without taking it off the team', () => {
    const after = setEnabled(team, 'coder', false)
    expect(after.map((r) => r.templateId)).toEqual(['planner', 'coder', 'reviewer'])
    expect(after.find((r) => r.templateId === 'coder')!.enabled).toBe(false)
    // Its position survives, which is the difference between parking a role
    // and unchecking it in the Roles column.
    expect(after.find((r) => r.templateId === 'coder')!.position).toBe(1)
  })

  it('turns one back on, the case that had no control at all', () => {
    const parked = setEnabled(team, 'coder', false)
    const back = setEnabled(parked, 'coder', true)
    expect(back.find((r) => r.templateId === 'coder')!.enabled).toBe(true)
  })

  it('moves the terminal role when the last one is parked', () => {
    expect(terminal(team)).toBe('reviewer')
    expect(terminal(setEnabled(team, 'reviewer', false))).toBe('coder')
  })

  it('leaves the others alone', () => {
    const after = setEnabled(team, 'coder', false)
    expect(after.filter((r) => r.enabled).map((r) => r.templateId)).toEqual(['planner', 'reviewer'])
  })
})
