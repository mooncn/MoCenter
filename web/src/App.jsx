import React, { useState, useEffect, useRef } from 'react'
import { Routes, Route, NavLink, Navigate, useLocation } from 'react-router-dom'
import Dashboard from './pages/Dashboard.jsx'
import Terminal from './pages/Terminal.jsx'
import PortMap from './pages/PortMap.jsx'
import Offline from './pages/Offline.jsx'
import Audit from './pages/Audit.jsx'
import Ops from './pages/Ops.jsx'
import Account from './pages/Account.jsx'
import Login from './pages/Login.jsx'
import { Api, MCSocket } from './api/api.js'

// 全局 WebSocket 上下文
export const SocketContext = React.createContext(null)

function Layout() {
  const [agents, setAgents] = useState([])
  const socketRef = useRef(null)

  useEffect(() => {
    // 拉取主机列表
    Api.agents().then((d) => setAgents(Array.isArray(d) ? d : [])).catch(() => {})

    // 建立 WebSocket，接收实时推送
    const token = localStorage.getItem('mc_token')
    const socket = new MCSocket({
      token,
      onAgentList: (list) => setAgents(Array.isArray(list) ? list : []),
    })
    socket.connect()
    socketRef.current = socket
    return () => socket.close()
  }, [])

  const logout = () => {
    localStorage.removeItem('mc_token')
    location.href = '/login'
  }

  return (
    <SocketContext.Provider value={socketRef.current}>
      <div className="layout">
        <aside className="sidebar">
          <div className="logo">🖥 MoCenter</div>
          <nav>
            <NavLink to="/" end>仪表盘</NavLink>
            <NavLink to="/terminal">远程终端</NavLink>
            <NavLink to="/portmap">端口映射</NavLink>
            <NavLink to="/ops">运维操作</NavLink>
            <NavLink to="/offline">离线机器</NavLink>
            <NavLink to="/audit">操作审计</NavLink>
            <NavLink to="/account">账号设置</NavLink>
          </nav>
          <div className="spacer" />
          <nav>
            <a onClick={logout} style={{ cursor: 'pointer' }}>退出登录</a>
          </nav>
        </aside>
        <div className="content">
          <Routes>
            <Route path="/" element={<Dashboard agents={agents} />} />
            <Route path="/terminal" element={<Terminal agents={agents} />} />
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
