import { ref, watch } from 'vue'

/**
 * Light, dark, or whatever the machine says.
 *
 * Three states rather than a boolean: "follow the system" is a real choice and
 * the one most people want, and a two-way switch cannot express it — it would
 * pin the theme the first time somebody pressed it, including to what it
 * already was.
 *
 * The class on <html> is the source of truth for CSS. index.html sets it from
 * this same key before the bundle arrives, because a page that boots light and
 * turns dark is worse than either.
 */
export type Theme = 'light' | 'dark' | 'system'

const KEY = 'zerg.theme'

function stored(): Theme {
  const v = localStorage.getItem(KEY)
  return v === 'light' || v === 'dark' || v === 'system' ? v : 'system'
}

export const theme = ref<Theme>(stored())

/** Whether the system is asking for dark, kept live so "system" follows it. */
const systemDark = ref(
  typeof window !== 'undefined' && window.matchMedia('(prefers-color-scheme: dark)').matches,
)
if (typeof window !== 'undefined') {
  window
    .matchMedia('(prefers-color-scheme: dark)')
    .addEventListener('change', (e) => (systemDark.value = e.matches))
}

/** What is actually on screen, which is what an icon should show. */
export function isDark(): boolean {
  return theme.value === 'dark' || (theme.value === 'system' && systemDark.value)
}

function apply() {
  document.documentElement.classList.toggle('dark', isDark())
}

watch([theme, systemDark], apply, { immediate: true })
watch(theme, (v) => {
  try {
    localStorage.setItem(KEY, v)
  } catch {
    // Private windows refuse this. The choice still holds for the session.
  }
})
