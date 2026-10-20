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

// renderFatal 渲染崩溃兜底面板。
//
// 为什么全部走 DOM API 而不是 innerHTML 模板（L-3）：这里的 detail 来自
// error.message / stack / 失败资源的 URL，而其中一部分内容最终源自被管主机的回传文本
// （服务端把 Agent 的原因原样带出来）。用 innerHTML 拼就等于把"未经转义的第三方内容"
// 交给 HTML 解析器 —— 现在只是被 CSP 的 script-src 'self' 兜住不能执行脚本，
// 但"靠另一层防护来原谅这一层的写法"正是会随某次调整（加白名单、代理吞头）失守的模式。
// textContent 让这件事根本不可能发生，同时按钮改用 addEventListener：
// 内联 onclick 在本 CSP 下是被禁止的，旧面板上那两个按钮其实点了没反应。
function renderFatal(title, detail) {
  if (typeof document === 'undefined') return
  const id = 'mc-fatal-overlay'
  if (document.getElementById(id)) return // 只显示第一个错误，避免叠成一团
  const box = document.createElement('div')
  box.id = id
  box.setAttribute('style', PANEL_CSS)
  const clip = (v) => String(v == null ? '' : v).slice(0, 2000)

  const add = (text, style) => {
    const el = document.createElement('div')
    el.setAttribute('style', style)
    el.textContent = text
    box.appendChild(el)
    return el
  }

  add(`⚠️ ${clip(title)}`, 'font-size:18px;font-weight:700;color:#ef4444')
  add('页面未能正常渲染。请把下面这段信息（或本页截图）反馈给管理员，可据此定位问题。', 'color:#8b98a5')

  const pre = document.createElement('pre')
  pre.setAttribute('style', 'margin:0;padding:12px;border-radius:8px;background:#1a2129;border:1px solid #2e3a46;white-space:pre-wrap;word-break:break-all;font-size:12px')
  pre.textContent = clip(detail)
  box.appendChild(pre)

  add(`UA：${clip(navigator.userAgent)}\n时间：${clip(new Date().toLocaleString())}`, 'color:#8b98a5;font-size:12px')

  const actions = document.createElement('div')
  actions.setAttribute('style', 'display:flex;gap:10px;flex-wrap:wrap')
  const mkButton = (label, style, onClick) => {
    const btn = document.createElement('button')
    btn.setAttribute('style', style)
    btn.textContent = label
    btn.addEventListener('click', onClick)
    actions.appendChild(btn)
  }
  mkButton('重新加载', 'background:#3b82f6;color:#fff;border:none;padding:9px 16px;border-radius:6px;font-size:14px;cursor:pointer', () => window.location.reload())
  mkButton('回到首页', 'background:#232c36;color:#e6edf3;border:1px solid #2e3a46;padding:9px 16px;border-radius:6px;font-size:14px;cursor:pointer', () => { window.location.href = '/' })
  box.appendChild(actions)

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
