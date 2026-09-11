import { describe, expect, it } from 'vitest'
import { langEntryFor } from '@/lib/highlight-langs'

describe('langEntryFor', () => {
  it('maps a common extension to its grammar', () => {
    expect(langEntryFor('src/main.ts')?.lang).toBe('typescript')
    expect(langEntryFor('src/App.vue')?.lang).toBe('vue')
    expect(langEntryFor('cmd/zerg/main.go')?.lang).toBe('go')
  })

  it('normalises aliases to one canonical grammar', () => {
    expect(langEntryFor('a.yml')?.lang).toBe('yaml')
    expect(langEntryFor('a.yaml')?.lang).toBe('yaml')
    expect(langEntryFor('a.mjs')?.lang).toBe('javascript')
    expect(langEntryFor('a.h')?.lang).toBe('c')
  })

  it('matches extensionless names by basename, case-insensitively', () => {
    expect(langEntryFor('Dockerfile')?.lang).toBe('dockerfile')
    expect(langEntryFor('path/to/Makefile')?.lang).toBe('makefile')
  })

  it('is null for anything not in the list, rather than guessing', () => {
    expect(langEntryFor('a.unknownext')).toBeNull()
    expect(langEntryFor('README')).toBeNull()
    expect(langEntryFor('noextension')).toBeNull()
  })

  it('does not read a leading dot as an extension separator', () => {
    // ".gitignore" has no extension by this rule's definition (dot must not
    // be the first character), so it must not be misread as an extension
    // "gitignore".
    expect(langEntryFor('.gitignore')).toBeNull()
  })
})
