import React, { useRef, useState, useEffect } from 'react'
import RFB from '@novnc/novnc'
import { Api } from '../api/api.js'

// VNC 远程桌面：通过服务端隧道连接目标主机内网 VNC，浏览器侧由 noVNC 渲染桌面。
export default function VncPage({ agents }) {
  const viewerRef = useRef(null)
  const rfbRef = useRef(null)
  const hostRef = useRef('')

  const [host, setHost] = useState('')
  const [port, setPort] = useState(5900)
  const [status, setStatus] = useState('idle') // idle/connecting/connected/disconnected
  const [notice, setNotice] = useState('')
  const [vncUp, setVncUp] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [password, setPassword] = useState('')

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
    rfb.resizeSession = true
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
      setNotice((r.ok ? '✅ ' : '❌ ') + (r.msg || '执行完成') + (r.password ? '（已获取口令）' : ''))
      if (r.password) { setPassword(r.password); sessionStorage.setItem(pwKey(), r.password) }
      setVncUp(!!r.ok)
    }
    try {
      let r = await Api.vncInstall({ agent_id: host, port })
      // 请求超时/未带口令时，轮询最近结果以补取口令
      if (!r.password) {
        for (let i = 0; i < 40; i++) {
          await new Promise((res) => setTimeout(res, 2000))
          try {
            r = await Api.vncStatus(host)
            if (r.password || r.ok) break
          } catch { /* retry */ }
        }
      }
      apply(r)
    } catch (ex) {
      setNotice('下发失败：' + (ex.response?.data?.msg || ex.message))
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

        <div className="vnc-wrap" style={{
          marginTop: 12, background: '#0b0e11', border: '1px solid #232a33', borderRadius: 8, minHeight: 420, overflow: 'auto',
        }}>
          <div ref={viewerRef} style={{ width: '100%', minHeight: 420 }} />
        </div>
        {notice && <div className="notice" style={{ marginTop: 10 }}>{notice}</div>}
      </div>
    </div>
  )
}