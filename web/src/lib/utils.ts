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
