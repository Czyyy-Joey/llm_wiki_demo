import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const apiTarget = process.env.VITE_API_PROXY_TARGET || 'http://127.0.0.1:8080'

export default defineConfig({ plugins: [react()], server: { port: 5173, proxy: { '/api': apiTarget } }, test: { environment: 'jsdom', setupFiles: './src/test-setup.ts', exclude: ['node_modules', 'tests/e2e/**'] } })
