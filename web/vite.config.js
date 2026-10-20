import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import fs from 'fs'
import path from 'path'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))

// noVNC 1.7.0 在模块顶层用 `await VideoDecoder.flush()` 做 H.264 自检
// （core/util/browser.js）。真机 Chrome Android 上 isConfigSupported 很快返回 true，
// 但 flush() 可能永不 settle —— 顶层 await 会卡住整个入口模块图，
// 页面只剩 index.html 的深色底，表现为“直接黑屏”。
//
// 这里把它改成**非阻塞**：先按“不支持 WebCodecs H.264”启动，
// 探测完成后再回填（桌面端仍可正常启用）；即使 flush 永远不返回，
// 也只是少了 H.264 编码，不会拖住首屏。
const NOVNC_WEBCODECS_AWAIT =
  'supportsWebCodecsH264Decode = await _checkWebCodecsH264DecodeSupport();'

function novncWebCodecsProbePatch() {
  return {
    name: 'mocenter-novnc-webcodecs-probe',
    enforce: 'pre',
    transform(code, id) {
      if (!id.replace(/\\/g, '/').includes('@novnc/novnc/core/util/browser.js')) return null
      if (!code.includes(NOVNC_WEBCODECS_AWAIT)) {
        console.warn('[vite] noVNC WebCodecs 自检补丁未命中（上游可能已修改），请确认首屏不再被顶层 await 卡住')
        return null
      }
      return code.replace(
        NOVNC_WEBCODECS_AWAIT,
        `supportsWebCodecsH264Decode = false;
void Promise.race([
    _checkWebCodecsH264DecodeSupport(),
    new Promise((resolve) => setTimeout(() => resolve(false), 800)),
]).then((supported) => {
    supportsWebCodecsH264Decode = supported;
});`,
      )
    },
  }
}

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
    plugins: [novncWebCodecsProbePatch(), react()],
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
      // 依赖分包：VNC(noVNC) 与终端(xterm) 各自成块并按需加载。
      // 公开状态页与后台首页不再为「可能用不到的功能」付下载代价 ——
      // 移动端弱网下这是首屏耗时的主要来源。
      rollupOptions: {
        output: {
          manualChunks(id) {
            if (!id.includes('node_modules')) return undefined
            if (id.includes('novnc')) return 'vendor-novnc'
            if (id.includes('xterm')) return 'vendor-xterm'
            if (id.includes('react') || id.includes('scheduler')) return 'vendor-react'
            return 'vendor'
          },
        },
      },
      chunkSizeWarningLimit: 600,
    },
    optimizeDeps: {
      // 让 dev 也走源码 transform，确保上面的 noVNC 补丁在开发模式下同样生效。
      exclude: ['@novnc/novnc'],
      esbuildOptions: { target: 'es2022' },
    },
  }
})