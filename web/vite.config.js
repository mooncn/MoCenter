import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import fs from 'fs'
import path from 'path'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))

// 从服务端配置文件提取 http 监听地址，作为后端候选目标。
// 同时兼容默认名 mocenter-server.json 与自定义名 cfg.json。
function targetsFromConfigs() {
  const root = path.resolve(__dirname, '..')
  const found = []
  const names = ['mocenter-server.json', 'cfg.json']
  const add = (dir) => {
    for (const name of names) {
      try {
        const cfg = JSON.parse(fs.readFileSync(path.join(dir, name), 'utf8'))
        if (typeof cfg.http === 'string') {
          const m = cfg.http.trim().match(/:(\d+)\s*$/)
          if (m) found.push(`http://localhost:${m[1]}`)
        }
      } catch { /* ignore */ }
    }
  }
  // 按优先级收集：项目根、build、_diag* 临时部署目录
  add(root)
  add(path.join(root, 'build'))
  try {
    for (const e of fs.readdirSync(root)) {
      if (/^_diag/.test(e)) add(path.join(root, e))
    }
  } catch { /* ignore */ }
  return [...new Set(found)]
}

// 依次探测候选后端，返回第一个能响应 /api/health 的在运行服务。
async function probeLive(targets) {
  for (const t of targets) {
    try {
      const ctrl = new AbortController()
      const timer = setTimeout(() => ctrl.abort(), 1000)
      const res = await fetch(t + '/api/health', { signal: ctrl.signal })
      clearTimeout(timer)
      if (res.ok) return t
    } catch { /* try next */ }
  }
  return null
}

async function discoverBackend() {
  // 显式指定的环境变量优先
  if (process.env.VITE_API_TARGET) return process.env.VITE_API_TARGET
  const candidates = targetsFromConfigs()
  const live = await probeLive(candidates)
  if (live) return live
  return candidates[0] || 'http://localhost:8080'
}

// Vite 配置支持异步（async factory），据此探测实际在运行的后端端口。
export default defineConfig(async () => {
  const apiTarget = await discoverBackend()
  console.log(`[vite] 后端代理目标: ${apiTarget}（可用环境变量 VITE_API_TARGET 覆盖）`)
  return {
    plugins: [react()],
    server: {
      port: 5173,
      proxy: {
        // 开发环境将 API 与 WebSocket 代理到服务端
        '/api': { target: apiTarget, changeOrigin: true },
        '/ws': { target: apiTarget.replace('http', 'ws'), ws: true },
        // VNC 查看器 WebSocket（noVNC）也需代理到服务端
        '/vnc-ws': { target: apiTarget.replace('http', 'ws'), ws: true },
      },
    },
    build: {
      outDir: 'dist',
      // noVNC 依赖顶层 await，需要更高 ES target
      target: 'es2022',
    },
    optimizeDeps: {
      esbuildOptions: { target: 'es2022' },
    },
  }
})