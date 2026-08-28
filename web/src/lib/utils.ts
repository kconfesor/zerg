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
