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

const spacing = `@@ -1,1 +1,1 @@
-const role = "admin user";
+const role = "adminuser";
`

const noNewline = `@@ -1,3 +1,3 @@
-one
\\ No newline at end of file
+one
+two
 three
`

const fold = (w: ReturnType<typeof mount>) =>
  w.findAll('button').find((b) => b.text().includes('whitespace-only'))

describe('a diff', () => {
  it('offers to fold a hunk that only moved the whitespace, and folds nothing until asked', async () => {
    // A reformat touching four hundred lines is the most common way a diff
    // becomes unreadable: the change worth reading is somewhere inside it. It
    // is still the reader who decides not to look: whitespace is syntax in
    // Python and in YAML, and hiding a change by default is the panel deciding
    // what a review does not need to see.
    const w = mount(DiffView, { props: { diff: reformat } })
    expect(w.text()).toContain('let x = 1;')
    expect(w.text()).not.toContain('lines hidden')

    await fold(w)!.trigger('click')
    expect(w.text()).toContain('whitespace only')
    expect(w.text()).toContain('lines hidden')
  })

  it('does not offer to fold a hunk where something actually changed', () => {
    // Same shape, one character different. Folding this would hide the change.
    const w = mount(DiffView, { props: { diff: real } })
    expect(fold(w)).toBeUndefined()
    expect(w.text()).toContain('let x = 2;')
  })

  // Compared with whitespace stripped rather than collapsed, "admin user" and
  // "adminuser" are the same string, and deleting a space inside a literal was
  // offered as a reformat.
  it('does not call a deleted space a whitespace change', () => {
    const w = mount(DiffView, { props: { diff: spacing } })
    expect(fold(w)).toBeUndefined()
  })

  // git's note about the line above is not a line of the file. Counted as
  // context it advanced both numbers and every remark below it pointed one
  // line too far down.
  it('does not number git\'s no-newline marker as a line', () => {
    const w = mount(DiffView, { props: { diff: noNewline } })
    expect(w.find('[aria-label="Comment on line 1"]').exists()).toBe(true)
    expect(w.find('[aria-label="Comment on line 2"]').exists()).toBe(true)
    expect(w.find('[aria-label="Comment on line 4"]').exists()).toBe(false)
  })

  it('shows the folded lines again when asked', async () => {
    const w = mount(DiffView, { props: { diff: reformat } })
    await fold(w)!.trigger('click')
    await w.findAll('button').find((b) => b.text().includes('lines hidden'))!.trigger('click')
    expect(w.text()).not.toContain('lines hidden')
    expect(w.text()).toContain('let x = 1;')
  })

  // A whole line painted red and its replacement green makes the reader diff
  // them by eye, which is the machine's job.
  it('marks only the token that changed inside a line pair', () => {
    const w = mount(DiffView, { props: { diff: real } })
    const hot = w.findAll('.rounded-\\[2px\\]').map((el) => el.text())
    expect(hot).toContain('2')
    expect(hot).toContain('1')
    expect(hot.join('')).not.toContain('let')
  })

  it('does not mark a rewrite as an edit', () => {
    const rewrite = `@@ -1,1 +1,1 @@
-const answer = compute(x);
+return nothing_like_it_at_all()
`
    const w = mount(DiffView, { props: { diff: rewrite } })
    expect(w.findAll('.rounded-\\[2px\\]').length).toBe(0)
  })

  // git's bookkeeping, translated: the reader wants where they are and what
  // function they are in, both already inside the marker.
  it('renders a hunk header as a place, not a marker', () => {
    const w = mount(DiffView, {
      props: { diff: '@@ -12,7 +12,9 @@ fn parse(s: &Scanner)\n x\n' },
    })
    expect(w.text()).toContain('lines 12-20 · fn parse(s: &Scanner)')
    expect(w.text()).not.toContain('@@ -12,7')
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
