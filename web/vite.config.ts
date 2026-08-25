import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
      // See src/lib/hugeicons.ts: shadcn-vue's generated components import
      // icon components from a package that only exports the renderer. The
      // alias fixes their imports without editing files the CLI owns.
      '@hugeicons/vue': fileURLToPath(new URL('./src/lib/hugeicons.ts', import.meta.url)),
    },
  },
  server: {
    // In dev the cockpit runs on its own port and the daemon on another, so
    // /api is proxied. In production the daemon serves the built assets from
    // the same origin and no proxy exists.
    proxy: { '/api': 'http://127.0.0.1:7717' },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
