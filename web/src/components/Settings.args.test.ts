import { describe, expect, it } from 'vitest'
import { joinArgs, splitArgs } from '@/lib/args'

/**
 * The ordering rule the settings form has to keep.
 *
 * Later wins in every CLI here, so moving a switch ahead of a hand-written flag
 * reverses what the operator wrote. This mirrors what Settings.vue does with
 * the edited text, at the level the behaviour actually lives.
 */
function setExtra(flags: string[], known: boolean[], text: string): string[] {
  const edited = splitArgs(text)
  const out: string[] = []
  let next = 0
  let lastExtra = -1
  for (let i = 0; i < flags.length; i++) {
    if (known[i]) {
      out.push(flags[i])
      continue
    }
    if (next < edited.length) {
      lastExtra = out.length
      out.push(edited[next++])
    }
  }
  out.splice(lastExtra + 1, 0, ...edited.slice(next))
  return out
}

describe('custom harness flags', () => {
  it('keeps a custom flag where it was, ahead of a known one', () => {
    const flags = ['--verbose', '--permission-mode', 'bypassPermissions']
    const known = [false, true, true]
    expect(setExtra(flags, known, joinArgs(['--verbose']))).toEqual(flags)
  })

  it('edits a custom flag in place rather than moving it to the end', () => {
    const flags = ['--verbose', '--permission-mode', 'bypassPermissions']
    const known = [false, true, true]
    expect(setExtra(flags, known, '--quiet')).toEqual([
      '--quiet',
      '--permission-mode',
      'bypassPermissions',
    ])
  })

  it('adds new custom flags beside the ones already there', () => {
    const flags = ['--verbose', '--strict-mcp-config']
    const known = [false, true]
    expect(setExtra(flags, known, '--verbose --quiet')).toEqual([
      '--verbose',
      '--quiet',
      '--strict-mcp-config',
    ])
  })

  it('drops custom flags that were removed from the text', () => {
    const flags = ['--verbose', '--quiet', '--strict-mcp-config']
    const known = [false, false, true]
    expect(setExtra(flags, known, '')).toEqual(['--strict-mcp-config'])
  })
})
