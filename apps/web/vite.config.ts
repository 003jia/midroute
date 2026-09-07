import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 18101,
    proxy: {
      '/api': { target: 'http://127.0.0.1:18100', changeOrigin: true },
      '/healthz': { target: 'http://127.0.0.1:18100', changeOrigin: true },
      '/readyz': { target: 'http://127.0.0.1:18100', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
  },
})