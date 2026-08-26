/**
 * Turning a list of arguments into editable text and back.
 *
 * This is a list of argv entries, not a shell command: no expansion, no
 * globbing, no operators. What it does have to survive is a round trip — the
 * text box is rendered from the stored list on every edit, so anything join
 * writes must split back to exactly what it was given, or editing one flag
 * silently rewrites another.
 */

/**
 * Split editable text into arguments.
 *
 * Quotes group, and a backslash inside a quoted run escapes the next character.
 * The escape is what makes the round trip closed: join has to be able to write
 * an argument containing a quote, and the only way to write one is to escape
 * it. The previous version quoted on the way out with JSON escaping and split
 * on the way back with a regex that did not understand escapes, so an argument
 * containing a quote came back as two arguments, both wrong.
 */
export function splitArgs(text: string): string[] {
  const out: string[] = []
  let cur = ''
  let has = false
  let quote: '"' | "'" | null = null

  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (quote && ch === '\\' && i + 1 < text.length) {
      cur += text[++i]
      has = true
      continue
    }
    if (quote) {
      if (ch === quote) quote = null
      else cur += ch
      has = true
      continue
    }
    if (ch === '"' || ch === "'") {
      quote = ch
      has = true
      continue
    }
    if (/\s/.test(ch)) {
      if (has) out.push(cur)
      cur = ''
      has = false
      continue
    }
    cur += ch
    has = true
  }
  if (has) out.push(cur)
  return out
}

/** The inverse: quote anything that would not split back to itself. */
export function joinArgs(args: string[]): string {
  return args
    .map((a) => {
      if (a === '') return '""'
      if (!/[\s"'\\]/.test(a)) return a
      return `"${a.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`
    })
    .join(' ')
}
