/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    // In dev the cockpit runs on its own port and the daemon on another, so
    // /api is proxied. In production the daemon serves the built assets from
    // the same origin and no proxy exists.
    proxy: { '/api': 'http://127.0.0.1:7717' },
    // `zerg up` fronts this dev server, and over Tailscale the browser's Host
    // is the tailnet MagicDNS name, which Vite blocks by default. A leading dot
    // allows any subdomain, so this covers every machine's ts.net name rather
    // than hard-coding one.
    allowedHosts: ['.ts.net'],
  },
  build: { outDir: 'dist', emptyOutDir: true },
  // Vite's default worker format is 'iife', which cannot code-split: every
  // dynamic import() inside the highlighter worker (one per Shiki grammar)
  // gets inlined into one file regardless, so opening any file at all paid
  // for every language this app might ever highlight. 'es' lets each
  // grammar land in its own chunk, fetched only once a file of that
  // language is actually opened. Measured against a real build: the
  // worker bundle dropped from 2.54 MB to 165 KB plus per-grammar chunks.
  worker: { format: 'es' },
  test: {
    // happy-dom rather than jsdom: these tests mount components and press
    // keys, which needs a DOM, and this one starts in a fraction of the time.
    environment: 'happy-dom',
    include: ['src/**/*.test.ts'],
  },
})
