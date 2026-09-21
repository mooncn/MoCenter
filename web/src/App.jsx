import React, { useState, useEffect, useRef, Suspense, lazy } from 'react'
import { Routes, Route, NavLink, Navigate, useLocation, Link } from 'react-router-dom'
import Dashboard from './pages/Dashboard.jsx'
import Terminal from './pages/Terminal.jsx'
import Vnc from './pages/Vnc.jsx'
import PortMap from './pages/PortMap.jsx'
import Offline from './pages/Offline.jsx'
import Audit from './pages/Audit.jsx'
import Ops from './pages/Ops.jsx'
import Account from './pages/Account.jsx'
import SiteConfig from './pages/SiteConfig.jsx'
import Latency from './pages/Latency.jsx'
import Deploy from './pages/Deploy.jsx'
import Backup from './pages/Backup.jsx'
import AgentEnroll from './pages/AgentEnroll.jsx'
import Login from './pages/Login.jsx'
import PublicStatus, { PublicNode } from './pages/PublicStatus.jsx'
import { Api, PublicApi, MCSocket } from './api/api.js'
import { applySiteTitle } from './utils/siteBranding.js'

// 全局 WebSocket 上下文
export const SocketContext = React.createContext(null)

// 侧边栏导航项（移动端抽屉与桌面端共用同一份定义）
// 管理后台统一挂在 /admin/* 前缀下：首页（/）留给公开状态页，
// 后台的"仪表盘"必须有自己的地址，否则登录后又被公开页顶掉。
const ADMIN = '/admin'
export const NAV_ITEMS = [
  { to: `${ADMIN}`, label: '仪表盘', icon: '📊', end: true },
  { to: `${ADMIN}/terminal`, label: '远程终端', icon: '💻' },
  { to: `${ADMIN}/latency`, label: '延迟监测', icon: '📶' },
  { to: `${ADMIN}/vnc`, label: '远程桌面', icon: '🖥' },
  { to: `${ADMIN}/portmap`, label: '端口映射', icon: '🔀' },
  { to: `${ADMIN}/ops`, label: '运维操作', icon: '🛠' },
  { to: `${ADMIN}/offline`, label: '离线机器', icon: '📴' },
  { to: `${ADMIN}/audit`, label: '操作审计', icon: '📋' },
  { to: `${ADMIN}/site-config`, label: '展示配置', icon: '🪧' },
  { to: `${ADMIN}/deploy`, label: '部署 Agent', icon: '🚀' },
  { to: `${ADMIN}/enroll`, label: 'Agent 接入', icon: '🔐' },
  { to: `${ADMIN}/backup`, label: '备份恢复', icon: '💾' },
  { to: `${ADMIN}/account`, label: '账号设置', icon: '⚙️' },
]
// 后台入口：登录成功后的默认落地页。
export const ADMIN_HOME = ADMIN

function Layout() {
  const [agents, setAgents] = useState([])
  const [navOpen, setNavOpen] = useState(false) // 移动端抽屉开关
  const socketRef = useRef(null)
  const location = useLocation()

  // 路由变化时自动收起移动端抽屉
  useEffect(() => { setNavOpen(false) }, [location.pathname])

  // 抽屉展开时锁定页面滚动，避免背景随手指滚动
  useEffect(() => {
    document.body.style.overflow = navOpen ? 'hidden' : ''
    return () => { document.body.style.overflow = '' }
  }, [navOpen])

  useEffect(() => {
    // 拉取主机列表
    Api.agents().then((d) => setAgents(Array.isArray(d) ? d : [])).catch(() => {})

    // 站点标题：后台页面同样使用「展示配置」里的标题。
    // 用免鉴权的 /api/branding 读取（它与公开页开关无关）—— 登录页与后台
    // 共用一份来源，避免"登录前一个标题、登录后又变回去"。
    PublicApi.branding().then((b) => applySiteTitle(b.site_title)).catch(() => {})

    // 备注保存后刷新主机列表，使备注在仪表盘即时生效
    const onRemarkSaved = () => Api.agents().then((d) => setAgents(Array.isArray(d) ? d : [])).catch(() => {})
    window.addEventListener('mc-remark-saved', onRemarkSaved)

    // 建立 WebSocket，接收实时推送
    const token = localStorage.getItem('mc_token')
    const socket = new MCSocket({
      token,
      onAgentList: (list) => setAgents(Array.isArray(list) ? list : []),
    })
    socket.connect()
    socketRef.current = socket
    return () => { socket.close(); window.removeEventListener('mc-remark-saved', onRemarkSaved) }
  }, [])

  // 登出：先通知服务端吊销当前会话 Token，再清除本地凭据
  const logout = async () => {
    try { await Api.logout() } catch { /* 即使请求失败也要清本地凭据 */ }
    localStorage.removeItem('mc_token')
    location.href = '/admin/login'
  }

  return (
    <SocketContext.Provider value={socketRef.current}>
      <div className={`layout ${navOpen ? 'nav-open' : ''}`}>
        {/* 移动端抽屉遮罩 */}
        <div className="nav-backdrop" onClick={() => setNavOpen(false)} />

        <aside className="sidebar">
          <div className="logo">
            <span>🖥 MoCenter</span>
            <button className="sidebar-close" onClick={() => setNavOpen(false)} aria-label="关闭菜单">✕</button>
          </div>
          <nav>
            {NAV_ITEMS.map((it) => (
              <NavLink key={it.to} to={it.to} end={it.end}>
                <span className="nav-icon">{it.icon}</span>{it.label}
              </NavLink>
            ))}
          </nav>
          <div className="spacer" />
          <nav>
            <a onClick={logout} style={{ cursor: 'pointer' }}><span className="nav-icon">🚪</span>退出登录</a>
          </nav>
        </aside>

        <div className="content">
          {/* 移动端顶部栏：菜单按钮 + 当前页面标题 */}
          <div className="mobile-topbar">
            <button className="menu-btn" onClick={() => setNavOpen(true)} aria-label="打开菜单">☰</button>
            <span className="mobile-title">
              {NAV_ITEMS.find((i) => (i.end ? location.pathname === i.to : location.pathname.startsWith(i.to)))?.label || 'MoCenter'}
            </span>
          </div>
          {/* fallback 给懒加载期间一个明确反馈，避免弱网下"点了没反应" */}
          <Suspense fallback={<div className="route-loading">正在加载页面…</div>}>
            <Routes>
              <Route path="/" element={<Dashboard agents={agents} />} />
              <Route path="/terminal" element={<Terminal agents={agents} />} />
              <Route path="/vnc" element={<Vnc agents={agents} />} />
              <Route path="/portmap" element={<PortMap agents={agents} />} />
              <Route path="/ops" element={<Ops agents={agents} />} />
              <Route path="/offline" element={<Offline agents={agents} />} />
              <Route path="/audit" element={<Audit />} />
              <Route path="/site-config" element={<SiteConfig />} />
              <Route path="/latency" element={<Latency agents={agents} />} />
              <Route path="/deploy" element={<Deploy />} />
              <Route path="/enroll" element={<AgentEnroll />} />
              <Route path="/backup" element={<Backup />} />
              <Route path="/account" element={<Account />} />
              {/* 兜底：未知后台路径回到仪表盘，而不是空白页 */}
              <Route path="*" element={<Navigate to={ADMIN} replace />} />
            </Routes>
          </Suspense>
        </div>
      </div>
    </SocketContext.Provider>
  )
}

function Guard({ children }) {
  const location = useLocation()
  const token = localStorage.getItem('mc_token')
  if (!token) {
    // 未登录访问后台：回登录页并记住原地址，登录后回到原地址（而不是公开页）
    return <Navigate to={`/admin/login?redirect=${encodeURIComponent(location.pathname + location.search)}`} replace />
  }
  return children
}

export default function App() {
  return (
    <Routes>
      {/* 首页 = 公开节点状态页（**免登录**）：打开站点默认看到的就是它。
          管理后台不再占用根路径，统一挂在 /admin/*（见下）。 */}
      <Route path="/" element={<PublicStatus />} />
      <Route path="/public" element={<PublicStatus />} />
      <Route path="/public/node/:nodeId" element={<PublicNode />} />
      {/* 后台登录：/login 保留为旧地址别名（书签/文档里可能仍是它） */}
      <Route path="/admin/login" element={<Login />} />
      <Route path="/login" element={<Navigate to="/admin/login" replace />} />
      {/* 管理后台：**仪表盘在 /admin**，其余页面在 /admin/<page>。
          Guard 保留原始路径交给 Layout，Layout 内部按相对路径匹配。 */}
      <Route
        path="/admin/*"
        element={
          <Guard>
            <Layout />
          </Guard>
        }
      />
      <Route path="/*" element={<LegacyRedirect />} />
    </Routes>
  )
}

// LegacyRedirect 处理管理后台的**根级旧地址**，其余未知路径给出 404。
//
// 背景：管理页曾挂在根路径（/terminal、/audit…），首页让给公开状态页后，
// 这些地址被原样交给管理后台渲染 —— 而 Layout 内部是按 `/`（仪表盘）匹配的，
// 于是登录后打开后台仪表盘，看到的却是公开状态页。现在旧地址一律重定向到
// 对应的 /admin 地址：既不丢书签，也不会再和公开页混淆。
function LegacyRedirect() {
  const location = useLocation()
  const legacy = [
    '/terminal', '/vnc', '/portmap', '/ops', '/offline', '/audit', '/site-config', '/account',
    '/latency', '/deploy', '/backup',
  ]
  const hit = legacy.find((p) => location.pathname === p || location.pathname.startsWith(p + '/'))
  if (hit) {
    const rest = location.pathname.slice(hit.length)
    return <Navigate to={`/admin${hit}${rest}${location.search}`} replace />
  }
  // 真正未知的地址：给出明确的 404 与回程入口（而不是重定向，
  // 否则打错的地址会被静默"纠正"，用户以为访问成功）
  return (
    <div className="public-shell">
      <div className="public-empty">
        <div className="public-empty-icon">🧭</div>
        <div className="public-empty-title">页面不存在</div>
        <div className="public-empty-actions">
          <Link className="public-btn" to="/">返回节点状态页</Link>
          <Link className="public-btn ghost" to="/admin">进入管理后台</Link>
        </div>
      </div>
    </div>
  )
}
