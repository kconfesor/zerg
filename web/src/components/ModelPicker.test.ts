import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ModelPicker from './ModelPicker.vue'
import type { Model } from '@/lib/api'

const models: Model[] = [
  { ID: 'claude-opus-5', Label: 'Opus 5', Provider: 'anthropic', Context: 200_000 },
  { ID: 'claude-sonnet-5', Label: 'Sonnet 5', Provider: 'anthropic', Context: 200_000 },
  { ID: 'openai-codex/gpt-5.6-sol', Label: 'gpt-5.6', Provider: 'openai', Context: 400_000 },
]

function picker(value = '') {
  return mount(ModelPicker, { props: { modelValue: value, models, label: 'Model' } })
}

describe('ModelPicker', () => {
  it('labels its input', () => {
    const w = picker()
    const id = w.get('input').attributes('id')
    expect(id).toBeTruthy()
    expect(w.get('label').attributes('for')).toBe(id)
  })

  it('announces itself as a combobox that is closed until focused', async () => {
    const w = picker()
    const input = w.get('input')
    expect(input.attributes('role')).toBe('combobox')
    expect(input.attributes('aria-expanded')).toBe('false')

    await input.trigger('focus')
    expect(input.attributes('aria-expanded')).toBe('true')
    expect(w.get('[role="listbox"]').attributes('id')).toBe(input.attributes('aria-controls'))
  })

  it('moves through the list with the arrow keys and says which option is active', async () => {
    const w = picker()
    const input = w.get('input')
    await input.trigger('focus')
    expect(input.attributes('aria-activedescendant')).toBeUndefined()

    await input.trigger('keydown', { key: 'ArrowDown' })
    const options = w.findAll('[role="option"]')
    expect(input.attributes('aria-activedescendant')).toBe(options[0].attributes('id'))

    await input.trigger('keydown', { key: 'ArrowDown' })
    expect(input.attributes('aria-activedescendant')).toBe(options[1].attributes('id'))

    await input.trigger('keydown', { key: 'ArrowUp' })
    expect(input.attributes('aria-activedescendant')).toBe(options[0].attributes('id'))
  })

  it('takes the active option with Enter, without a mouse', async () => {
    const w = picker()
    const input = w.get('input')
    await input.trigger('focus')
    await input.trigger('keydown', { key: 'ArrowDown' })
    await input.trigger('keydown', { key: 'Enter' })

    expect(w.emitted('update:modelValue')?.at(-1)).toEqual(['claude-opus-5'])
    expect(w.emitted('commit')?.at(-1)).toEqual(['claude-opus-5'])
    expect(input.attributes('aria-expanded')).toBe('false')
  })

  it('narrows to what has been typed, matched anywhere in the id', async () => {
    const w = picker('sol')
    await w.get('input').trigger('focus')
    const options = w.findAll('[role="option"]')
    expect(options).toHaveLength(1)
    expect(options[0].text()).toContain('openai-codex/gpt-5.6-sol')
  })

  it('closes on Escape without changing anything', async () => {
    const w = picker('claude')
    const input = w.get('input')
    await input.trigger('focus')
    await input.trigger('keydown', { key: 'ArrowDown' })
    await input.trigger('keydown', { key: 'Escape' })

    expect(input.attributes('aria-expanded')).toBe('false')
    expect(w.emitted('update:modelValue')).toBeUndefined()
    expect(w.emitted('commit')).toBeUndefined()
  })

  it('keeps what was typed when nothing was picked', async () => {
    // The list narrows; it does not constrain. A model a catalog has not heard
    // of still has to reach the daemon.
    const w = picker('some-model-nobody-lists')
    const input = w.get('input')
    await input.trigger('focus')
    await input.trigger('blur')
    expect(w.emitted('commit')?.at(-1)).toEqual(['some-model-nobody-lists'])
  })
})
