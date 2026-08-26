import { reactive } from 'vue'

/**
 * Which operations are in flight, so a control that has been pressed can be
 * disabled until it answers.
 *
 * Every mutation here is a round trip with no optimistic update, so the button
 * looks untouched for as long as the daemon takes — which invites a second
 * press. A second Start is refused with an error the operator did not cause; a
 * second "Queue it" opens a duplicate card; a second Approve races the first
 * through an integration that runs git.
 *
 * Keyed rather than a single boolean: two different cards can be acted on at
 * once, and one card's pending delete must not grey out the other's.
 */
export function usePending() {
  const busy = reactive(new Set<string>())

  return {
    /** Whether this operation is waiting on the daemon. */
    is: (key: string) => busy.has(key),

    /**
     * Run an operation once, ignoring re-entry while it is in flight.
     *
     * Returns undefined when it was already running, so a caller can tell a
     * skipped press from a completed one.
     */
    async run<T>(key: string, work: () => Promise<T>): Promise<T | undefined> {
      if (busy.has(key)) return undefined
      busy.add(key)
      try {
        return await work()
      } finally {
        busy.delete(key)
      }
    },
  }
}
