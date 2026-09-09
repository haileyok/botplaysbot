import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    // Dev-server proxy to the Go appview so `pnpm --filter web dev` works
    // against a locally running stack (the embedded build needs none of this).
    proxy: {
      '/xrpc': {
        target: 'http://localhost:8080',
        ws: true,
      },
      '/api': {
        target: 'http://localhost:8080',
      },
      '/.well-known': {
        target: 'http://localhost:8080',
      },
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    globals: true,
    setupFiles: ['src/test-setup.ts'],
  },
})
