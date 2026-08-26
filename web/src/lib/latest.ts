/**
 * A guard for "only the newest answer counts".
 *
 * Every view here fetches per project, and a project switch does not cancel
 * what is already in flight. The older set can land after the newer one and
 * put one project's data under another project's name — briefly, and exactly
 * while someone is looking to see whether the switch worked.
 *
 * Comparing the id it was asked for is not enough. A → B → A leaves two
 * requests for A outstanding; the first can land last, and the id still
 * matches, so a stale answer is committed as if it were current. A sequence
 * number is the thing that actually distinguishes them: each call takes the
 * next number, and only the call holding the highest one may write.
 *
 *   const newest = latest()
 *   async function load() {
 *     const current = newest()
 *     const data = await api.thing()
 *     if (!current()) return
 *     value.value = data
 *   }
 */
export function latest(): () => () => boolean {
  let seq = 0
  return () => {
    const mine = ++seq
    return () => mine === seq
  }
}
