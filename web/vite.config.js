import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 后端地址可通过环境变量 VITE_API_TARGET 范围调整，默认 http://localhost:8080
const apiTarget = process.env.VITE_API_TARGET || 'http://localhost:8080'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // 开发环境将 API 与 WebSocket 代理到服务端
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/ws': {
        target: apiTarget.replace('http', 'ws'),
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
  },
})
