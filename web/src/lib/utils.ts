import type { ClassValue } from "clsx"
import { clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/** Formats a token count the way the spend views do: 1.37M, 240K, 75. */
export function tokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`
  return String(n)
}

/**
 * Money, to as many places as the number needs.
 *
 * Three decimals under a dollar: agent turns cost fractions of a cent, and
 * rounding them to two places makes a column of real costs read as $0.00.
 */
export function money(n: number): string {
  if (n === 0) return '$0'
  return n >= 1 ? `$${n.toFixed(2)}` : `$${n.toFixed(3)}`
}

/** Formats a duration in milliseconds as 2h 41m or 44m. */
export function duration(ms: number): string {
  const mins = Math.round(ms / 60_000)
  if (mins < 60) return `${mins}m`
  return `${Math.floor(mins / 60)}h ${String(mins % 60).padStart(2, '0')}m`
}

/**
 * The models that did the work, short enough to sit on a card.
 *
 * "claude-sonnet-5" and "gpt-5.6-sol" are the identifiers, and the vendor
 * prefix is the least interesting part of them on a board where every card
 * carries one. The full names are in the title, since the short form is
 * ambiguous the moment two vendors ship a "5".
 */
export function shortModel(model: string): string {
  return model.replace(/^(claude|openai|anthropic|google)-/, '')
}

/** The CLI that will work the next card through this lane: claude or pi. */
export function laneHarness(
  team: { enabled: boolean; name: string; harness: string }[],
  lane: string,
): string {
  return team.find((r) => r.enabled && r.name === lane)?.harness ?? ''
}

/**
 * Which CLI a card's harness icon should draw, and pulse when it is the one
 * working.
 *
 * A queued or working card is claimed by whoever's lane it sits in right now,
 * which can differ from the first harness that ever touched it — a card that
 * started under claude's planner and hands off to a pi coder must not go on
 * pulsing claude's mark while pi is the one actually spending tokens. A done
 * (or otherwise finished) card has left every lane, so the only truth left is
 * the last harness recorded against it — the one that produced what is on the
 * card now, not whichever ran first.
 */
export function taskHarness(
  team: { enabled: boolean; name: string; harness: string }[],
  task: { state: string; lane: string; harnesses?: string[] },
): string {
  if (task.state === 'queued' || task.state === 'working') {
    return laneHarness(team, task.lane) || task.harnesses?.[0] || ''
  }
  return task.harnesses?.[task.harnesses.length - 1] || laneHarness(team, task.lane)
}

/**
 * What a card's state should be called.
 *
 * `rejected` is stored for two different events — a role turned the work down,
 * and a person parked it — because widening the stored states would have meant
 * rebuilding a table whose deletes cascade through every transcript. The
 * timestamp is what separates them, and this is the one place that knows it.
 */
export function taskState(task: { state: string; stoppedAt?: string }): string {
  return task.state === 'rejected' && task.stoppedAt ? 'stopped' : task.state
}

/**
 * Where a finished card actually lands, in this project.
 *
 * Three settings, and only one of them merges anything: a project can open a
 * pull request, or leave the work on its branch and land nothing at all. The
 * pipeline used to end with "merges to main" whatever the project said, which
 * is a claim about someone's repository that was simply false two thirds of the
 * time.
 *
 * `head` names the last column of a diagram; `line` is the sentence for a rail.
 */
export function landing(project: {
  integration: string
  prDraft?: boolean
  baseBranch: string
}): { head: string; line: string } {
  switch (project.integration) {
    case 'pr':
      return project.prDraft
        ? { head: 'draft PR', line: `opens a draft pull request into ${project.baseBranch}` }
        : { head: 'pull request', line: `opens a pull request into ${project.baseBranch}` }
    case 'branch':
      return { head: 'its branch', line: 'stays on its branch, landing it is your call' }
    default:
      return { head: project.baseBranch, line: `merges to ${project.baseBranch}` }
  }
}
