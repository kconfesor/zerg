/**
 * Syntax-highlights one file, off the main thread.
 *
 * A byte cutoff bounds what gets fetched, not what gets highlighted: a
 * native regex engine backtracking badly is a property of a pattern meeting
 * adversarial input, not of file size, so a size limit alone would not have
 * stopped a short, deliberately pathological line from hanging a tokenizer.
 * The actual guard is that this runs somewhere the tab can terminate --
 * lib/highlight.ts does, on a deadline. See
 * docs/design/code-explorer.md decision 6.
 */
import { createHighlighterCore, type HighlighterCore } from 'shiki/core'
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript'
import { langEntryFor } from '@/lib/highlight-langs'

let core: Promise<HighlighterCore> | null = null

function highlighter(): Promise<HighlighterCore> {
  core ??= createHighlighterCore({
    themes: [import('@shikijs/themes/github-light'), import('@shikijs/themes/github-dark')],
    langs: [],
    engine: createJavaScriptRegexEngine(),
  })
  return core
}

export interface Request {
  id: number
  path: string
  code: string
}
export type Response = { id: number; html: string | null } | { id: number; error: string }

self.onmessage = async (e: MessageEvent<Request>) => {
  const { id, path, code } = e.data
  try {
    const entry = langEntryFor(path)
    if (!entry) {
      self.postMessage({ id, html: null } satisfies Response)
      return
    }
    const shiki = await highlighter()
    if (!shiki.getLoadedLanguages().includes(entry.lang)) {
      // highlight-langs.ts stays Shiki-agnostic (it is shared with the main
      // thread), so its loader is typed as a bare Promise; this is the one
      // place that needs to tell TypeScript what shiki already knows at
      // runtime -- that every entry in it is a valid grammar module.
      await shiki.loadLanguage(entry.load() as Parameters<typeof shiki.loadLanguage>[0])
    }
    const html = shiki.codeToHtml(code, {
      lang: entry.lang,
      themes: { light: 'github-light', dark: 'github-dark' },
    })
    self.postMessage({ id, html } satisfies Response)
  } catch (err) {
    self.postMessage({ id, error: err instanceof Error ? err.message : String(err) } satisfies Response)
  }
}
