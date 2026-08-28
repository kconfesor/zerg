import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import DiffView from './DiffView.vue'

const reformat = `@@ -1,3 +1,3 @@
-fn parse(s: &str) {
-  let x = 1;
-}
+fn parse(s: &str) {
+    let x = 1;
+}
`

const real = `@@ -1,3 +1,3 @@
-fn parse(s: &str) {
-  let x = 1;
-}
+fn parse(s: &str) {
+  let x = 2;
+}
`

describe('a diff', () => {
  it('folds a hunk that only moved the whitespace', () => {
    // A reformat touching four hundred lines is the most common way a diff
    // becomes unreadable: the change worth reading is somewhere inside it.
    const w = mount(DiffView, { props: { diff: reformat } })
    expect(w.text()).toContain('whitespace only')
    expect(w.text()).toContain('lines hidden')
  })

  it('does not fold a hunk where something actually changed', () => {
    // Same shape, one character different. Folding this would hide the change.
    const w = mount(DiffView, { props: { diff: real } })
    expect(w.text()).not.toContain('whitespace only')
    expect(w.text()).toContain('let x = 2;')
  })

  it('shows the folded lines when asked', async () => {
    const w = mount(DiffView, { props: { diff: reformat } })
    await w.findAll('button').find((b) => b.text().includes('whitespace only'))!.trigger('click')
    expect(w.text()).not.toContain('lines hidden')
    expect(w.text()).toContain('let x = 1;')
  })

  it('offers each line as somewhere to comment', async () => {
    const w = mount(DiffView, { props: { diff: real } })
    const gutter = w.get('[aria-label="Comment on line 2"]')
    await gutter.trigger('click')

    const [line, hunk] = w.emitted('comment')![0] as [number, string]
    expect(line).toBe(2)
    // The hunk goes with it, removals included: a question about "this" needs
    // the change, not only the result.
    expect(hunk).toContain('+  let x = 2;')
    expect(hunk).toContain('-  let x = 1;')
  })
})
