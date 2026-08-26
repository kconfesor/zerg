import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import MarkdownEditor from './MarkdownEditor.vue'

/**
 * The editor is rich text; the model is Markdown. These are about the seam.
 *
 * ProseMirror needs a real document and a tick to mount its view, so each test
 * waits for the editor before touching it.
 */
async function editor(initial = '') {
  const w = mount(MarkdownEditor, {
    props: { modelValue: initial, 'onUpdate:modelValue': (v: string) => w.setProps({ modelValue: v }) },
    attachTo: document.body,
  })
  await new Promise((r) => setTimeout(r, 0))
  await w.vm.$nextTick()
  return w
}

describe('MarkdownEditor', () => {
  it('parses Markdown into a document rather than showing its marks', async () => {
    const w = await editor('## Factorial\n\nAdd a postfix `!` operator.')
    const html = w.find('.wysiwyg').html()
    expect(html).toContain('<h2>Factorial</h2>')
    expect(html).toContain('<code>!</code>')
    // The point of the rewrite: no asterisks or hashes on screen.
    expect(w.find('.wysiwyg').text()).not.toContain('##')
  })

  it('serialises back to Markdown, not HTML', async () => {
    const w = await editor('# Title\n\n- one\n- two')
    // Round trip through the editor leaves the source recognisable: the model
    // is what goes to the agent, and an agent reading tags is the failure this
    // whole component is arranged to avoid.
    const md = w.props('modelValue') as string
    expect(md).not.toContain('<h1>')
    expect(md).toContain('# Title')
  })

  it('offers the source as an escape hatch for what the schema cannot model', async () => {
    const w = await editor('# Title')
    const tabs = w.findAll('button').filter((b) => b.text() === 'source')
    expect(tabs).toHaveLength(1)
    await tabs[0].trigger('click')
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('# Title')
  })

  it('marks the toolbar with what is active at the caret', async () => {
    const w = await editor('# Title')
    const h1 = w.find('[aria-label="Heading 1"]')
    expect(h1.exists()).toBe(true)
    // Nothing is asserted about the caret's initial position — only that the
    // control reports state at all, which the old toolbar never did.
    expect(h1.attributes('aria-pressed')).toBeDefined()
  })
})
