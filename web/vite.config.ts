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
  },
  build: { outDir: 'dist', emptyOutDir: true },
  test: {
    // happy-dom rather than jsdom: these tests mount components and press
    // keys, which needs a DOM, and this one starts in a fraction of the time.
    environment: 'happy-dom',
    include: ['src/**/*.test.ts'],
  },
})
