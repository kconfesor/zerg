import type { Attention } from '@/lib/api'

/**
 * What the queue is holding, by kind: "2 approvals · 1 question".
 *
 * The panel is one queue with three different things in it, and a terminal
 * approval brings a file browser and a whole diff with it. A question sitting
 * under two of those is off the bottom of the screen, and the only clue it
 * exists is a number on the bell that does not say what kind of thing it is
 * counting. Naming the kinds at the top is how you know to keep scrolling.
 *
 * Empty when nothing is waiting: the panel already says "Nothing needs you"
 * in that case, and a count of nothing is worse than silence.
 */
export function summarizeAttention(a: Attention | null): string {
  if (!a) return ''
  const parts: string[] = []
  const add = (n: number, one: string, many: string) => {
    if (n > 0) parts.push(`${n} ${n === 1 ? one : many}`)
  }
  // The same words the cards themselves are badged with, so the summary reads
  // as a count of what is below it rather than a second vocabulary.
  add(a.approvals.length, 'approval', 'approvals')
  add(a.plans?.length ?? 0, 'plan', 'plans')
  add(a.features?.length ?? 0, 'feature', 'features')
  add(a.stalls?.length ?? 0, 'stalled feature', 'stalled features')
  add(a.clarifications.length, 'question', 'questions')
  add(a.rework.tasks.length, 'looping card', 'looping cards')
  return parts.join(' · ')
}
