import { ref, type Ref } from 'vue'

/**
 * Dragging a row to another place in a list.
 *
 * Pointer events rather than HTML5 drag and drop, which has no touch support at
 * all, and rather than a library: reka-ui, which shadcn-vue is built on here,
 * has no sortable primitive, and the usual answers weigh some 45KB for what is
 * written below. Shared because two lists reorder pipelines, the Team screen's
 * column and the rail beside the board, and pointer capture and the off-by-one
 * are not things to get right twice.
 *
 * The list does not reorder while the pointer moves. Callers draw a line where
 * the row will land and dim the row being carried, so the thing under the
 * pointer stays the thing that was grabbed and nothing shuffles out from under
 * it.
 */
export function useRowDrag(source: {
  /** The element holding the rows, each marked `data-role-row`. */
  container: Ref<HTMLElement | null>
  /** How many rows there are, for the line after the last one. */
  count: () => number
  /** Where it was dropped. `to` counts the gaps in the list as it stands. */
  onDrop: (from: number, to: number) => void
}) {
  const drag = ref<{ from: number; to: number } | null>(null)
  let midpoints: number[] = []

  function grab(event: PointerEvent, index: number) {
    const rows = [...(source.container.value?.querySelectorAll('[data-role-row]') ?? [])]
    midpoints = rows.map((row) => {
      const box = row.getBoundingClientRect()
      return box.top + box.height / 2
    })
    drag.value = { from: index, to: index }
    const handle = event.currentTarget as HTMLElement
    handle.setPointerCapture(event.pointerId)
    // Touch scrolls the page otherwise, and the row goes nowhere while the list
    // slides past underneath it. preventDefault also cancels the focus a click
    // would have given the handle, which is where the arrow keys are read, so
    // the focus is put back by hand: pressing a handle and then an arrow key
    // did nothing at all.
    event.preventDefault()
    handle.focus()
  }

  function dragTo(event: PointerEvent) {
    if (!drag.value) return
    let to = midpoints.findIndex((mid) => event.clientY < mid)
    if (to === -1) to = midpoints.length
    drag.value = { ...drag.value, to }
  }

  function drop() {
    const move = drag.value
    drag.value = null
    if (move) source.onDrop(move.from, move.to)
  }

  /** Whether the line goes above this row, or below the last one. */
  function dropsBefore(index: number): boolean {
    const move = drag.value
    return !!move && move.to === index && move.from !== index && move.from !== index - 1
  }
  function dropsLast(index: number): boolean {
    const move = drag.value
    return !!move && index === source.count() - 1 && move.to === source.count() && move.from !== index
  }

  return { drag, grab, dragTo, drop, dropsBefore, dropsLast }
}
