/**
 * Shim for shadcn-vue's hugeicons output.
 *
 * shadcn-vue 2.8.2 generates `import { Tick02Icon } from '@hugeicons/vue'` and
 * renders `<Tick02Icon />`. That package (1.0.8, latest) exports only
 * HugeiconsIcon and HugeiconsPlugin — icon data lives in
 * @hugeicons/core-free-icons and is passed to the renderer as a prop, never
 * used as a component. The generated code does not compile as written.
 *
 * Rather than hand-edit files the CLI owns — which the next `shadcn-vue add`
 * would regenerate, reintroducing the bug — vite and tsconfig alias
 * '@hugeicons/vue' to this module. The generated components stay byte-identical
 * to what the CLI produced and their imports resolve here.
 *
 * The icon data is a plain [tag, attrs] list, so this renders it directly
 * rather than importing the real renderer: importing '@hugeicons/vue' from
 * inside the module that aliases it is circular by construction.
 *
 * Only the icons those components use are wrapped. If a future `add` pulls in
 * another, the build fails with a missing export naming it — the right way to
 * find out.
 */
import { defineComponent, h, type PropType } from 'vue'
import {
  ArrowDown01Icon as ArrowDown01,
  ArrowUp01Icon as ArrowUp01,
  Cancel01Icon as Cancel01,
  Tick02Icon as Tick02,
} from '@hugeicons/core-free-icons'

type IconNode = [string, Record<string, string>]

/** SVG attributes are kebab-case; the icon data spells them camelCase. */
function kebab(key: string): string {
  return key.replace(/[A-Z]/g, (c) => '-' + c.toLowerCase())
}

function wrap(name: string, nodes: unknown) {
  const parts = nodes as IconNode[]
  return defineComponent({
    name,
    props: { size: { type: [Number, String] as PropType<number | string>, default: 24 } },
    setup(props, { attrs }) {
      return () =>
        h(
          'svg',
          {
            xmlns: 'http://www.w3.org/2000/svg',
            viewBox: '0 0 24 24',
            width: props.size,
            height: props.size,
            fill: 'none',
            'aria-hidden': 'true',
            ...attrs,
          },
          parts.map(([tag, raw]) => {
            const attrsOut: Record<string, string> = {}
            for (const [k, v] of Object.entries(raw)) {
              if (k === 'key') continue
              attrsOut[kebab(k)] = v
            }
            return h(tag, attrsOut)
          }),
        )
    },
  })
}

export const ArrowDown01Icon = wrap('ArrowDown01Icon', ArrowDown01)
export const ArrowUp01Icon = wrap('ArrowUp01Icon', ArrowUp01)
export const Cancel01Icon = wrap('Cancel01Icon', Cancel01)
export const Tick02Icon = wrap('Tick02Icon', Tick02)
