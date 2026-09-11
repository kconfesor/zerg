/**
 * Which Shiki grammar a file's name asks for, and how to load it.
 *
 * Fine-grained rather than the bundled-everything highlighter: a .go file
 * never pulls in the CSS grammar. Only the languages a project here is
 * likely to hold -- extended when a real one shows up unhighlighted, not
 * grown speculatively.
 */
export interface LangEntry {
  lang: string
  load: () => Promise<unknown>
}

const BY_EXT: Record<string, LangEntry> = {
  ts: { lang: 'typescript', load: () => import('@shikijs/langs/typescript') },
  tsx: { lang: 'tsx', load: () => import('@shikijs/langs/tsx') },
  js: { lang: 'javascript', load: () => import('@shikijs/langs/javascript') },
  mjs: { lang: 'javascript', load: () => import('@shikijs/langs/javascript') },
  cjs: { lang: 'javascript', load: () => import('@shikijs/langs/javascript') },
  jsx: { lang: 'jsx', load: () => import('@shikijs/langs/jsx') },
  vue: { lang: 'vue', load: () => import('@shikijs/langs/vue') },
  go: { lang: 'go', load: () => import('@shikijs/langs/go') },
  rs: { lang: 'rust', load: () => import('@shikijs/langs/rust') },
  py: { lang: 'python', load: () => import('@shikijs/langs/python') },
  rb: { lang: 'ruby', load: () => import('@shikijs/langs/ruby') },
  java: { lang: 'java', load: () => import('@shikijs/langs/java') },
  c: { lang: 'c', load: () => import('@shikijs/langs/c') },
  h: { lang: 'c', load: () => import('@shikijs/langs/c') },
  cpp: { lang: 'cpp', load: () => import('@shikijs/langs/cpp') },
  cc: { lang: 'cpp', load: () => import('@shikijs/langs/cpp') },
  hpp: { lang: 'cpp', load: () => import('@shikijs/langs/cpp') },
  cs: { lang: 'csharp', load: () => import('@shikijs/langs/csharp') },
  php: { lang: 'php', load: () => import('@shikijs/langs/php') },
  css: { lang: 'css', load: () => import('@shikijs/langs/css') },
  scss: { lang: 'scss', load: () => import('@shikijs/langs/scss') },
  html: { lang: 'html', load: () => import('@shikijs/langs/html') },
  xml: { lang: 'xml', load: () => import('@shikijs/langs/xml') },
  json: { lang: 'json', load: () => import('@shikijs/langs/json') },
  jsonc: { lang: 'jsonc', load: () => import('@shikijs/langs/jsonc') },
  yaml: { lang: 'yaml', load: () => import('@shikijs/langs/yaml') },
  yml: { lang: 'yaml', load: () => import('@shikijs/langs/yaml') },
  toml: { lang: 'toml', load: () => import('@shikijs/langs/toml') },
  md: { lang: 'markdown', load: () => import('@shikijs/langs/markdown') },
  markdown: { lang: 'markdown', load: () => import('@shikijs/langs/markdown') },
  sql: { lang: 'sql', load: () => import('@shikijs/langs/sql') },
  sh: { lang: 'shellscript', load: () => import('@shikijs/langs/shellscript') },
  bash: { lang: 'bash', load: () => import('@shikijs/langs/bash') },
  zsh: { lang: 'shellscript', load: () => import('@shikijs/langs/shellscript') },
  diff: { lang: 'diff', load: () => import('@shikijs/langs/diff') },
  ini: { lang: 'ini', load: () => import('@shikijs/langs/ini') },
}

/** Names git tracks with no extension. */
const BY_BASENAME: Record<string, LangEntry> = {
  dockerfile: { lang: 'dockerfile', load: () => import('@shikijs/langs/dockerfile') },
  makefile: { lang: 'makefile', load: () => import('@shikijs/langs/makefile') },
  gnumakefile: { lang: 'makefile', load: () => import('@shikijs/langs/makefile') },
}

/** The grammar for a repository path, or null for anything not in the list
 *  above -- which stays plain text rather than guessing. */
export function langEntryFor(path: string): LangEntry | null {
  const base = (path.split('/').pop() ?? path).toLowerCase()
  if (base in BY_BASENAME) return BY_BASENAME[base]
  const dot = base.lastIndexOf('.')
  const ext = dot > 0 ? base.slice(dot + 1) : ''
  return BY_EXT[ext] ?? null
}
