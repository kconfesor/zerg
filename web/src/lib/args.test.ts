import { describe, expect, it } from 'vitest'
import { joinArgs, splitArgs } from './args'

/**
 * The property that matters is the round trip. The text box is rendered from
 * the stored list on every edit, so anything join writes has to split back to
 * exactly what it was given — otherwise editing one flag rewrites another.
 */
describe('args', () => {
  const cases: string[][] = [
    [],
    ['--strict-mcp-config'],
    ['--permission-mode', 'bypassPermissions'],
    ['--flag', 'two words'],
    ['--flag', 'say "hello"'],
    ['--flag', "it's fine"],
    ['--path', 'C:\\Users\\someone'],
    ['--empty', ''],
    ['--mixed', 'a "b" c\\d'],
  ]

  for (const args of cases) {
    it(`round-trips ${JSON.stringify(args)}`, () => {
      expect(splitArgs(joinArgs(args))).toEqual(args)
    })
  }

  it('groups quoted runs and ignores surrounding whitespace', () => {
    expect(splitArgs('  --flag   "two words"  --other ')).toEqual(['--flag', 'two words', '--other'])
  })

  it('reads a quote that was escaped on the way out', () => {
    // The old splitter used a regex with no notion of escapes, so this came
    // back as two arguments, both wrong.
    expect(splitArgs('--flag "say \\"hello\\""')).toEqual(['--flag', 'say "hello"'])
  })

  it('leaves a plain flag unquoted', () => {
    expect(joinArgs(['--no-extensions'])).toBe('--no-extensions')
  })
})
