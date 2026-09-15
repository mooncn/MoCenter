import React, { useState, useEffect, useRef } from 'react'
import { Routes, Route, NavLink, Navigate, useLocation } from 'react-router-dom'
import Dashboard from './pages/Dashboard.jsx'
import Terminal from './pages/Terminal.jsx'
import Vnc from './pages/Vnc.jsx'
import PortMap from './pages/PortMap.jsx'
import Offline from './pages/Offline.jsx'
import Audit from './pages/Audit.jsx'
import Ops from './pages/Ops.jsx'
import Account from './pages/Account.jsx'
import Login from './pages/Login.jsx'
import { Api, MCSocket } from './api/api.js'

// 全局 WebSocket 上下文
export const SocketContext = React.createContext(null)

// 侧边栏导航项（移动端抽屉与桌面端共用同一份定义）
const NAV_ITEMS = [
  { to: '/', label: '仪表盘', icon: '📊', end: true },
  { to: '/terminal', label: '远程终端', icon: '💻' },
  { to: '/vnc', label: '远程桌面', icon: '🖥' },
  { to: '/portmap', label: '端口映射', icon: '🔀' },
  { to: '/ops', label: '运维操作', icon: '🛠' },
  { to: '/offline', label: '离线机器', icon: '📴' },
  { to: '/audit', label: '操作审计', icon: '📋' },
  { to: '/account', label: '账号设置', icon: '⚙️' },
]

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
    location.href = '/login'
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
          <Routes>
            <Route path="/" element={<Dashboard agents={agents} />} />
            <Route path="/terminal" element={<Terminal agents={agents} />} />
            <Route path="/vnc" element={<Vnc agents={agents} />} />
            <Route path="/portmap" element={<PortMap agents={agents} />} />
            <Route path="/ops" element={<Ops agents={agents} />} />
            <Route path="/offline" element={<Offline agents={agents} />} />
            <Route path="/audit" element={<Audit />} />
            <Route path="/account" element={<Account />} />
          </Routes>
        </div>
      </div>
    </SocketContext.Provider>
  )
}

function Guard() {
  const location = useLocation()
  const token = localStorage.getItem('mc_token')
  if (!token) {
    return <Navigate to="/login" state={{ from: location }} replace />
  }
  return <Layout />
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/*" element={<Guard />} />
    </Routes>
  )
}
