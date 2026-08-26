import React, { useContext, useRef, useState, useEffect } from 'react'
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

  const onlineHosts = (agents || []).filter((a) => a.online)

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

    const rfb = new RFB(viewerRef.current, buildWsUrl(), { credentials: { password: '' } })
    rfb.scaleViewport = true
    rfb.resizeSession = true
    rfbRef.current = rfb

    setStatus('connecting')
    rfb.addEventListener('connect', () => { setStatus('connected'); setNotice('') })
    rfb.addEventListener('disconnect', (e) => {
      setStatus('disconnected')
      // 非用户主动断开，视为连接失败（可能 VNC 未就绪或被断开）
      if (e?.detail?.clean === false) {
        setNotice('连接已断开。若目标未安装 VNC，请先执行“一键安装”。')
      }
    })
    rfb.addEventListener('securityfailure', (e) => {
      setNotice('安全协商失败：' + (e?.detail?.reason || '需要登录密码'))
    })
  }

  // 连接桌面：先探测可达性，再建立查看器
  const connect = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
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

  // 一键安装/启动 VNC
  const install = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
    setInstalling(true)
    setNotice('正在下发安装/启动指令，请稍候...')
    try {
      const r = await Api.vncInstall({ agent_id: host, port })
      setNotice((r.ok ? '✅ ' : '❌ ') + (r.msg || '执行完成'))
      setVncUp(!!r.ok)
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