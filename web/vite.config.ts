import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // Dev convenience: forward XRPC and WS calls to the local AppView.
      '/xrpc': {
        target: 'http://localhost:8080',
        ws: true,
      },
    },
  },
})
