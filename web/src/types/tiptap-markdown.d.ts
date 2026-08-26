/**
 * `tiptap-markdown` ships types for its own extension but never augments
 * TipTap's `Storage`, so `editor.storage.markdown` — the whole point of the
 * package — is an error at the call site. Declared here rather than cast at
 * each of the three uses, so the shape is stated once and checked.
 */
import type { MarkdownStorage } from 'tiptap-markdown'

declare module '@tiptap/core' {
  interface Storage {
    markdown: MarkdownStorage
  }
}
