import React, { useRef, useState, useEffect } from 'react'
import RFB from '@novnc/novnc'
import { Api } from '../api/api.js'

// VNC 远程桌面：通过服务端隧道连接目标主机内网 VNC，浏览器侧由 noVNC 渲染桌面。
export default function VncPage({ agents }) {
  const viewerRef = useRef(null)
  const wrapRef = useRef(null)
  const rfbRef = useRef(null)
  const hostRef = useRef('')

  const [host, setHost] = useState('')
  const [port, setPort] = useState(5900)
  const [status, setStatus] = useState('idle') // idle/connecting/connected/disconnected
  const [notice, setNotice] = useState('')
  const [vncUp, setVncUp] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [password, setPassword] = useState('')
  const [fullscreen, setFullscreen] = useState(false)
  // 连接工具栏（右下角）展开状态：窄屏默认收起，避免遮挡桌面
  const [toolsOpen, setToolsOpen] = useState(false)

  // 连接成功后，右下角工具栏默认展开，方便直接使用全屏
  useEffect(() => {
    if (status === 'connected') setToolsOpen(true)
  }, [status])

  const onlineHosts = (agents || []).filter((a) => a.online)

  // 口令按(主机,端口)存于 sessionStorage，会话结束即销毁
  const pwKey = () => `mc_vnc_pw_${host}_${port}`
  useEffect(() => {
    setPassword(sessionStorage.getItem(pwKey()) || '')
  }, [host, port])

  useEffect(() => {
    hostRef.current = host
  }, [host])

  // 卸载时断开 noVNC
  useEffect(() => {
    return () => { if (rfbRef.current) { try { rfbRef.current.disconnect() } catch {} rfbRef.current = null } }
  }, [])

  const buildWsUrl = () => {
    const token = localStorage.getItem('mc_token') || ''
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    return `${proto}://${location.host}/vnc-ws?agent=${encodeURIComponent(hostRef.current)}&port=${port}&token=${encodeURIComponent(token)}`
  }

  const openViewer = () => {
    if (!viewerRef.current) return
    if (rfbRef.current) { try { rfbRef.current.disconnect() } catch {} rfbRef.current = null }

    const rfb = new RFB(viewerRef.current, buildWsUrl(), { credentials: { password } })
    rfb.scaleViewport = true
    rfb.resizeSession = false
    rfbRef.current = rfb

    setStatus('connecting')
    setNotice('正在连接 VNC 桌面，等待 RFB 握手/口令认证...')
    rfb.addEventListener('connect', () => { setStatus('connected'); setNotice('') })
    rfb.addEventListener('disconnect', (e) => {
      setStatus('disconnected')
      if (e?.detail?.clean === false) {
        setNotice('连接已断开。若口令有误，请核对后重试；若未安装 VNC，请先一键安装。')
      }
    })
    rfb.addEventListener('securityfailure', (e) => {
      setNotice('认证失败：' + (e?.detail?.reason || 'VNC 口令不正确或需要口令'))
    })
    // 服务端在握手中索要口令时补交
    rfb.addEventListener('credentialsrequired', () => {
      rfb.sendCredentials({ password })
    })
  }

  // 全屏：优先使用 Fullscreen API（真正隐藏浏览器 UI），
  // 不支持时回退为「沉浸式」样式（固定铺满视口并由 noVNC 自适应缩放）。
  const enterFullscreen = () => {
    const el = wrapRef.current
    const rfb = rfbRef.current
    if (!el) return
    const isFs = document.fullscreenElement || document.webkitFullscreenElement
    if (!isFs) {
      try {
        const req = el.requestFullscreen || el.webkitRequestFullscreen || el.msRequestFullscreen
        const ret = req && req.call(el)
        if (ret && typeof ret.then === 'function') ret.catch(() => {})
      } catch { /* 回退沉浸式 */ }
    }
    setFullscreen(true)
    // 容器尺寸变化后让 noVNC 重新按视口缩放
    setTimeout(() => { resizeViewer(rfb) }, 120)
  }

  const exitFullscreen = () => {
    const isFs = document.fullscreenElement || document.webkitFullscreenElement
    if (isFs) {
      try {
        const exit = document.exitFullscreen || document.webkitExitFullscreen || document.msExitFullscreen
        exit && exit.call(document)
      } catch { /* ignore */ }
    }
    setFullscreen(false)
    setTimeout(() => { resizeViewer(rfbRef.current) }, 120)
  }

  const toggleFullscreen = () => (fullscreen ? exitFullscreen() : enterFullscreen())

  // resizeViewer 触发 noVNC 重新计算缩放比例（切换全屏/旋转屏幕后需要）
  const resizeViewer = (rfb) => {
    if (!rfb) return
    try {
      rfb.scaleViewport = true
      if (typeof rfb._screenSize === 'object') rfb._screenSize = null
      window.dispatchEvent(new Event('resize'))
    } catch { /* ignore */ }
  }

  // 同步浏览器全屏状态（用户按 Esc / 切换全屏时保持一致）
  useEffect(() => {
    const onChange = () => {
      const isFs = !!(document.fullscreenElement || document.webkitFullscreenElement)
      setFullscreen(isFs)
      setTimeout(() => resizeViewer(rfbRef.current), 120)
    }
    document.addEventListener('fullscreenchange', onChange)
    document.addEventListener('webkitfullscreenchange', onChange)
    return () => {
      document.removeEventListener('fullscreenchange', onChange)
      document.removeEventListener('webkitfullscreenchange', onChange)
    }
  }, [])

  // 全屏时按 Esc 也退出沉浸式模式
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape' && fullscreen) exitFullscreen()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })

  // 连接桌面：先探测可达性，再建立查看器
  const connect = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
    if (!password) { setNotice('请输入 VNC 口令（或先“一键安装VNC”获取）'); return }
    setNotice('正在探测 VNC 可达性...')
    try {
      const probe = await Api.vncProbe(host, port)
      if (!probe.ok) { setVncUp(false); setStatus('disconnected'); setNotice('VNC 未就绪：' + probe.msg + '（可点击“一键安装VNC”自动安装/启动）'); return }
      setVncUp(true)
      openViewer()
    } catch (ex) {
      setVncUp(false)
      setNotice('探测失败：' + (ex.response?.data?.msg || ex.message))
    }
  }

  const disconnect = () => {
    if (rfbRef.current) { try { rfbRef.current.disconnect() } catch {} rfbRef.current = null }
    setStatus('disconnected')
    setNotice('')
  }

  // 一键安装/启动 VNC；成功后自动回填服务端下发的口令。
  // 若安装较慢导致本次请求超时，轮询 /vnc/status 补取口令。
  const install = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
    setInstalling(true)
    setNotice('正在下发安装/启动指令，请稍候...')
    const apply = (r) => {
      setNotice((r.ok ? '✅ ' : '❌ ') + (r.msg || r.message || '执行完成') + (r.password ? '（已获取口令）' : ''))
      if (r.password) { setPassword(r.password); sessionStorage.setItem(pwKey(), r.password) }
      setVncUp(!!r.ok)
    }
    // 轮询 /vnc/status 补取安装结果（含口令），最多等 5 分钟
    const pollStatus = async () => {
      for (let i = 0; i < 60; i++) {
        await new Promise((res) => setTimeout(res, 3000))
        try {
          const r = await Api.vncStatus(host)
          if (r && (r.password || r.ok)) return r
          // 安装明确失败时也提前退出
          if (r && r.ok === false && r.msg && /失败|错误|不支持/.test(r.msg)) return r
        } catch { /* retry */ }
      }
      return null
    }
    try {
      let r = await Api.vncInstall({ agent_id: host, port })
      // 请求成功但未带口令时，轮询补取
      if (!r.password) {
        r = await pollStatus() || r
      }
      apply(r)
    } catch (ex) {
      // 请求超时或网络错误：安装可能仍在后台进行，轮询补取结果
      setNotice('请求超时或网络异常，正在轮询安装结果...')
      const r = await pollStatus()
      if (r) {
        apply(r)
      } else {
        setNotice('下发失败：' + (ex.response?.data?.msg || ex.message) + '。可稍后点击"一键安装VNC"重试，或以管理员身份运行 Agent。')
      }
    } finally {
      setInstalling(false)
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>远程桌面 (VNC)</h2>
          <div className="sub">通过服务端隧道连接目标主机的 VNC，支持 Windows / Linux</div>
        </div>
        <span className={`badge ${status === 'connected' ? 'online' : status === 'connecting' ? 'idle' : 'offline'}`}>
          {status === 'connected' ? '已连接' : status === 'connecting' ? '连接中…' : status === 'disconnected' ? '已断开' : '未连接'}
        </span>
      </div>

      <div className="card">
        <div className="form-grid">
          <div className="form-row">
            <label>目标主机</label>
            <select value={host} onChange={(e) => { setHost(e.target.value); setStatus('idle'); setNotice('') }}>
              <option value="">请选择在线主机...</option>
              {onlineHosts.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>{a.hostname} ({a.agent_id})</option>
              ))}
            </select>
          </div>
          <div className="form-row">
            <label>VNC 端口</label>
            <input type="number" value={port} min={1} max={65535} onChange={(e) => setPort(Number(e.target.value))} style={{ width: 160 }} />
          </div>
          <div className="form-row">
            <label>VNC 口令</label>
            <input type="password" value={password} placeholder="一键安装后自动填充" onChange={(e) => setPassword(e.target.value)} style={{ width: 220 }} autoComplete="off" />
          </div>
        </div>

        <div className="terminal-toolbar">
          <button className="primary" onClick={connect} disabled={!host || status === 'connecting'}>连接桌面</button>
          <button className="secondary danger" onClick={install} disabled={!host || installing}>{installing ? '安装中…' : '一键安装VNC'}</button>
          {vncUp && <button className="secondary" onClick={disconnect} disabled={status === 'disconnected' || status === 'idle'}>断开</button>}
        </div>

        <div
          ref={wrapRef}
          className={`vnc-wrap ${fullscreen ? 'vnc-fullscreen' : ''}`}
        >
          <div ref={viewerRef} style={{ width: '100%', height: '100%', minHeight: 320 }} />

          {/* 右下角悬浮工具条：全屏 / 缩放 / 快捷键 */}
          <div className={`vnc-fab ${toolsOpen ? 'open' : ''}`}>
            {toolsOpen && (
              <div className="vnc-fab-menu">
                <button className="vnc-fab-item" onClick={toggleFullscreen} title="全屏显示桌面（Esc 退出）">
                  {fullscreen ? '⤡ 退出全屏' : '⛶ 全屏'}
                </button>
                <button className="vnc-fab-item" onClick={() => { const r = rfbRef.current; if (r) r.scaleViewport = !r.scaleViewport }} title="切换自适应缩放 / 原始分辨率">
                  ⇱ 自适应缩放
                </button>
                <button className="vnc-fab-item" onClick={() => { const r = rfbRef.current; if (r) r.sendCtrlAltDel() }} title="发送 Ctrl+Alt+Del">
                  ⌨ Ctrl+Alt+Del
                </button>
                <button className="vnc-fab-item" onClick={() => setToolsOpen(false)}>✕ 收起</button>
              </div>
            )}
            <button
              className="vnc-fab-toggle"
              onClick={() => setToolsOpen(!toolsOpen)}
              title={toolsOpen ? '收起工具' : '展开工具（全屏等）'}
              aria-label="VNC 工具栏"
            >
              {toolsOpen ? '✕' : (fullscreen ? '⤡' : '⛶')}
            </button>
          </div>

          {status !== 'connected' && (
            <div className="vnc-overlay">
              <div>
                <div style={{ fontSize: 15, fontWeight: 600, marginBottom: 6 }}>{status === 'connecting' ? '正在连接…' : '未连接'}</div>
                <div style={{ fontSize: 12, color: 'var(--muted)' }}>连接成功后右下角可开启全屏</div>
              </div>
            </div>
          )}
        </div>
        {notice && <div className="notice" style={{ marginTop: 10 }}>{notice}</div>}
      </div>
    </div>
  )
}