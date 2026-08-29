import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import Chat from './Chat.vue'
import { api, type Chat as Conversation, type Project } from '@/lib/api'

// The stream is a socket in the real thing; here it is a handle that does
// nothing, because these tests are about what happens to a file on its way out.
vi.mock('@/lib/api', () => ({
  api: {
    chats: vi.fn(),
    newChat: vi.fn(),
    chatAttach: vi.fn(),
    chat: vi.fn(),
    interruptChat: vi.fn(),
    endChat: vi.fn(),
    renameChat: vi.fn(),
    setChatAgent: vi.fn(),
  },
  streamActivity: () => ({ close: () => {} }),
  artifactBytes: (id: string) => `/api/artifacts/${id}/bytes`,
}))
enableAutoUnmount(afterEach)

const project = { id: 'P1', name: 'arto', baseBranch: 'main' } as Project
const conversation: Conversation = {
  id: 'C1',
  projectId: 'P1',
  title: 'the parser',
  createdAt: '2026-01-01T00:00:00Z',
  lastUsedAt: '2026-01-01T00:00:00Z',
}

beforeEach(() => {
  vi.mocked(api.chats).mockResolvedValue([conversation])
})

async function open() {
  const w = mount(Chat, { props: { project, harnesses: ['claude'], models: {} } })
  await flushPromises()
  return w
}

/** Pastes one file the way a screenshot arrives: in items, with no name. */
async function paste(w: Awaited<ReturnType<typeof open>>, file: File) {
  const items = [{ kind: 'file', getAsFile: () => file }]
  await w.find('textarea').trigger('paste', {
    clipboardData: { files: [], items },
  })
  await flushPromises()
}

describe('pasting a file into a chat', () => {
  // A pasted image has no filename, and a multipart part with no filename is a
  // form value rather than a file: the daemon answered "attach a file under the
  // form field file" for a picture that was right there on screen.
  it('sends something pasted under a name, since it arrives without one', async () => {
    vi.mocked(api.chatAttach).mockResolvedValue({
      id: 'A1',
      name: 'pasted.png',
    } as Awaited<ReturnType<typeof api.chatAttach>>)

    const w = await open()
    await paste(w, new File([new Uint8Array([1, 2, 3])], '', { type: 'image/png' }))

    expect(api.chatAttach).toHaveBeenCalledTimes(1)
    const sent = vi.mocked(api.chatAttach).mock.calls[0][2]
    expect(sent.name).not.toBe('')
    expect(sent.name).toMatch(/\.png$/)
    expect(sent.type).toBe('image/png')
  })

  // The row pushed onto the list is not the row the template renders: the ref
  // wraps it, and writing to the original changed the data without telling
  // anything to look again. An upload that had finished, and been recorded, sat
  // on screen saying "sending…" for as long as the tab was open.
  it('stops saying it is sending once the upload lands', async () => {
    vi.mocked(api.chatAttach).mockResolvedValue({
      id: 'A1',
      name: 'diagram.png',
    } as Awaited<ReturnType<typeof api.chatAttach>>)

    const w = await open()
    await paste(w, new File([new Uint8Array([1])], 'diagram.png', { type: 'image/png' }))

    expect(w.text()).toContain('diagram.png')
    expect(w.text()).not.toContain('sending')
    // And the message can now be sent, which is the point of waiting.
    const ask = w.findAll('button').find((b) => b.text() === 'Ask')
    expect(ask?.attributes('disabled')).toBeUndefined()
  })

  // A failure has to say so rather than looking like it is still working.
  it('says when an upload failed', async () => {
    vi.mocked(api.chatAttach).mockRejectedValue(new Error('that file is over the 25 MB limit'))

    const w = await open()
    await paste(w, new File([new Uint8Array([1])], 'huge.mov', { type: 'video/quicktime' }))

    expect(w.text()).toContain('failed')
    expect(w.text()).not.toContain('sending')
  })
})
