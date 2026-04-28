import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import path from 'node:path'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src')
    }
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:5310'
    }
  },
  build: {
    // Build directly to the backend-embedded webui assets directory.
    outDir: '../cli/webui/dist',
    emptyOutDir: true
  }
})
