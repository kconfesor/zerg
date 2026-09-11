/**
 * Highlights one file's content, off the main thread and on a deadline.
 *
 * The worker is terminated, not just ignored, if it does not answer in
 * time -- a hung highlight would otherwise keep running (and keep a loaded
 * grammar in memory) for every file opened after it. The deadline is the
 * actual guard against a pathological grammar; see
 * docs/design/code-explorer.md decision 6 and the worker's own comment.
 */
import type { Request, Response } from '@/workers/highlight.worker'

export type HighlightResult = { html: string | null } | { timedOut: true } | { error: string }

const DEADLINE_MS = 800

let worker: Worker | null = null
let nextId = 0
const pending = new Map<number, (msg: { html: string | null } | { error: string }) => void>()

function spawn(): Worker {
  const w = new Worker(new URL('../workers/highlight.worker.ts', import.meta.url), { type: 'module' })
  w.onmessage = (e: MessageEvent<Response>) => {
    pending.get(e.data.id)?.('error' in e.data ? { error: e.data.error } : { html: e.data.html })
    pending.delete(e.data.id)
  }
  return w
}

export function highlight(path: string, code: string): Promise<HighlightResult> {
  // Not worth a worker round trip for something this small to have gone
  // wrong in -- the timeout below is what matters for real files.
  if (code.trim().length < 20) return Promise.resolve({ html: null })

  worker ??= spawn()
  const mine = worker
  const id = nextId++
  const req: Request = { id, path, code }

  return new Promise<HighlightResult>((settle) => {
    let done = false
    const timer = setTimeout(() => {
      if (done) return
      done = true
      pending.delete(id)
      // Terminate the worker this request actually ran on, not whatever
      // `worker` currently points at -- a later file can already be running
      // on a fresh worker by the time this fires, and killing that one would
      // cut off work this timeout has nothing to do with.
      mine.terminate()
      if (worker === mine) worker = null
      settle({ timedOut: true })
    }, DEADLINE_MS)

    pending.set(id, (msg) => {
      if (done) return
      done = true
      clearTimeout(timer)
      settle(msg)
    })

    mine.postMessage(req)
  })
}
