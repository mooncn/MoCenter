import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App.jsx'
import './index.css'

// ============================================================
// 崩溃兜底：任何"白屏/黑屏"都必须能被用户看见原因
// ============================================================
//
// 背景：移动端出现"打开就是一片空白（黑屏）"时，如果连一句提示都没有，
// 运维只能看到"页面坏了"，既无法自助排查，也没法把有用信息反馈回来。
// 这里做三件事：
//
//   1. window.onerror / unhandledrejection 捕获运行时异常；
//   2. ErrorBoundary 捕获 React 渲染期异常；
//   3. 两者都渲染一个**自带深色底**的错误面板 —— 不依赖 index.css，
//      因为一旦样式表没加载成功，错误面板自己也会变成"白底白字"。
//
// 面板上同时显示 UA 与时间：移动端问题的第一手信息（机型/系统/浏览器版本）
// 只有出问题的设备才知道，让用户截图即可，不必再教他们开远程调试。

const PANEL_CSS = [
  'position:fixed', 'inset:0', 'z-index:2147483647',
  'display:flex', 'flex-direction:column', 'gap:12px',
  'padding:20px', 'box-sizing:border-box', 'overflow:auto',
  'background:#0f1419', 'color:#e6edf3', 'font-size:14px', 'line-height:1.6',
  "font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif",
].join(';')

function renderFatal(title, detail) {
  if (typeof document === 'undefined') return
  const id = 'mc-fatal-overlay'
  if (document.getElementById(id)) return // 只显示第一个错误，避免叠成一团
  const box = document.createElement('div')
  box.id = id
  box.setAttribute('style', PANEL_CSS)
  const safe = (v) => String(v == null ? '' : v).slice(0, 2000)
  box.innerHTML = `
    <div style="font-size:18px;font-weight:700;color:#ef4444">⚠️ ${safe(title)}</div>
    <div style="color:#8b98a5">页面未能正常渲染。请把下面这段信息（或本页截图）反馈给管理员，可据此定位问题。</div>
    <pre style="margin:0;padding:12px;border-radius:8px;background:#1a2129;border:1px solid #2e3a46;white-space:pre-wrap;word-break:break-all;font-size:12px">${safe(detail)}</pre>
    <div style="color:#8b98a5;font-size:12px">UA：${safe(navigator.userAgent)}<br/>时间：${safe(new Date().toLocaleString())}</div>
    <div style="display:flex;gap:10px;flex-wrap:wrap">
      <button style="background:#3b82f6;color:#fff;border:none;padding:9px 16px;border-radius:6px;font-size:14px;cursor:pointer" onclick="location.reload()">重新加载</button>
      <button style="background:#232c36;color:#e6edf3;border:1px solid #2e3a46;padding:9px 16px;border-radius:6px;font-size:14px;cursor:pointer" onclick="location.href='/'">回到首页</button>
    </div>`
  document.body.appendChild(box)
}

window.addEventListener('error', (e) => {
  // 资源加载失败（img/script 等）也会走到这里，target 非 window
  const where = e.target && e.target !== window && e.target.src ? `资源加载失败：${e.target.src}` : ''
  renderFatal('页面脚本异常', [e.message, where, e.error && e.error.stack].filter(Boolean).join('\n'))
})
window.addEventListener('unhandledrejection', (e) => {
  const r = e.reason
  renderFatal('页面异步请求异常', (r && (r.stack || r.message)) || String(r))
})

// ErrorBoundary 必须是 class：React 只支持类组件捕获渲染异常
class ErrorBoundary extends React.Component {
  constructor(props) {
    super(props)
    this.state = { error: null }
  }

  static getDerivedStateFromError(error) {
    return { error }
  }

  componentDidCatch(error, info) {
    console.error('[MoCenter] 渲染异常', error, info)
  }

  render() {
    if (this.state.error) {
      const e = this.state.error
      return (
        <div style={{ position: 'fixed', inset: 0, background: '#0f1419', color: '#e6edf3', padding: 20, overflow: 'auto' }}>
          <div style={{ fontSize: 18, fontWeight: 700, color: '#ef4444' }}>⚠️ 界面渲染失败</div>
          <div style={{ color: '#8b98a5', margin: '8px 0 12px' }}>
            请把下面这段信息（或本页截图）反馈给管理员，可据此定位问题。
          </div>
          <pre style={{
            margin: 0, padding: 12, borderRadius: 8, background: '#1a2129',
            border: '1px solid #2e3a46', whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12,
          }}>{String(e && (e.stack || e.message) || e)}</pre>
          <div style={{ color: '#8b98a5', fontSize: 12, marginTop: 10 }}>
            UA：{navigator.userAgent}<br />时间：{new Date().toLocaleString()}
          </div>
          <button
            style={{ marginTop: 14, background: '#3b82f6', color: '#fff', border: 'none', padding: '9px 16px', borderRadius: 6, fontSize: 14, cursor: 'pointer' }}
            onClick={() => window.location.reload()}
          >重新加载</button>
        </div>
      )
    }
    return this.props.children
  }
}

const root = document.getElementById('root')

// 入口为空时也必须说话：index.html 被改坏、构建产物不匹配、
// 或根节点被别的脚本清空时，用户看到的同样是"一片空白"。
if (!root) {
  renderFatal('页面初始化失败', '未找到 #root 挂载节点，index.html 可能已损坏。')
} else {
  try {
    ReactDOM.createRoot(root).render(
      <React.StrictMode>
        <ErrorBoundary>
          <BrowserRouter>
            <App />
          </BrowserRouter>
        </ErrorBoundary>
      </React.StrictMode>,
    )
  } catch (e) {
    renderFatal('页面初始化失败', (e && (e.stack || e.message)) || String(e))
  }
}
