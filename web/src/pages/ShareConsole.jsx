import React, { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import RFB from '@novnc/novnc'
import { ShareApi, MCSocket } from '../api/api.js'

// 分享访客页：链接形如 /share#<token>。
//
// token 放在 URL 片段里而不是查询参数：片段不会随请求发给服务端，
// 也不会出现在反向代理访问日志里。页面本身不需要登录，能力范围完全由
// 服务端按 token 校验（见 server/share_api.go），前端只负责展示与操作。

function readToken() {
  const raw = (window.location.hash || '').replace(/^#/, '')
  return raw.trim()
}

function fmtTime(ts) {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString()
}

const FEATURE_LABELS = {
  terminal: '远程终端',
  vnc: '远程桌面',
  portmap: '端口转发',
}

export default function ShareConsole() {
  const [token] = useState(readToken)
  const [info, setInfo] = useState(null)
  const [err, setErr] = useState('')
  const [expired, setExpired] = useState(false)
  const [tab, setTab] = useState('')

  const load = () => {
    if (!token) { setErr('链接缺少凭据：请使用发送方提供的完整链接'); return }
    setErr('')
    ShareApi.info(token)
      .then((d) => {
        if (!d?.ok) { setErr(d?.msg || '无法读取分享信息'); return }
        setInfo(d)
        const first = ['terminal', 'vnc', 'portmap'].find((f) => (d.features || []).includes(f)) || ''
        setTab((cur) => (cur && (d.features || []).includes(cur) ? cur : first))
      })
      .catch((e) => {
        if (e?.response?.status === 401) setExpired(true)
        setErr(e?.response?.data?.msg || e?.response?.data || e.message || '无法读取分享信息')
      })
  }

  useEffect(() => { load() }, [token])

  if (expired) {
    return (
      <ShareShell>
        <div className="card share-empty">
          <div style={{ fontSize: 40 }}>⌛</div>
          <h2>分享链接无效或已过期</h2>
          <p className="muted">
            该链接可能已到期、被撤销，或主机已从服务端注销。请联系分享人重新生成一条链接。
          </p>
        </div>
      </ShareShell>
    )
  }

  if (!info) {
    return (
      <ShareShell>
        <div className="card share-empty">
          <div className="muted">{err || '正在验证分享链接…'}</div>
          {err && <button className="secondary" style={{ marginTop: 12 }} onClick={load}>重试</button>}
        </div>
      </ShareShell>
    )
  }

  const features = info.features || []
  const hostName = info.remark || info.hostname || info.agent_id

  return (
    <ShareShell>
      <div className="card share-host">
        <div className="share-host-head">
          <div>
            <h1>{hostName}</h1>
            <div className="sub">
              {info.hostname} · <code>{info.agent_id}</code>
              {info.os_type ? ` · ${info.os_type}` : ''}
            </div>
          </div>
          <span className={`badge ${info.online ? 'online' : 'offline'}`}>{info.online ? '主机在线' : '主机离线'}</span>
        </div>
        <div className="share-meta">
          {info.label ? <span>备注：{info.label}</span> : null}
          <span>有效期至 {fmtTime(info.expires_at)}</span>
          <span>可用功能：{features.map((f) => FEATURE_LABELS[f] || f).join(' / ')}</span>
        </div>
        {!info.online && (
          <div className="notice err" style={{ marginTop: 10, marginBottom: 0 }}>
            目标主机当前离线，终端与桌面暂时无法连接；请等主机上线后再试。
          </div>
        )}
      </div>

      {features.length > 1 && (
        <div className="share-tabs">
          {features.map((f) => (
            <button
              key={f}
              className={tab === f ? 'active' : ''}
              onClick={() => setTab(f)}
            >{FEATURE_LABELS[f] || f}</button>
          ))}
        </div>
      )}

      <div className="card">
        {tab === 'terminal' && (
          <TerminalPanel token={token} agentId={info.agent_id} online={info.online} />
        )}
        {tab === 'vnc' && (
          <VncPanel token={token} info={info} />
        )}
        {tab === 'portmap' && (
          <PortmapPanel token={token} />
        )}
        {!tab && <p className="muted">这条链接没有授予任何可用功能。</p>}
      </div>
    </ShareShell>
  )
}

function ShareShell({ children }) {
  return (
    <div className="share-shell">
      <header className="share-top">
        <span className="share-brand">🖥 MoCenter · 远程协助</span>
        <span className="muted" style={{ fontSize: 12 }}>凭分享链接访问，无需登录</span>
      </header>
      <main className="share-main">{children}</main>
      <footer className="share-foot muted">
        本页面只在链接有效期内可用；分享人可随时撤销，撤销后已打开的连接会立即断开。
      </footer>
    </div>
  )
}

// ---------------- 远程终端 ----------------

function TerminalPanel({ token, agentId, online }) {
  const termRef = useRef(null)
  const termObj = useRef(null)
  const fitRef = useRef(null)
  const sockRef = useRef(null)
  const sessRef = useRef(null)
  // 会话 id 在命令回执（HTTP）之后才落到 sessRef，但远端 PTY 可能在回执前就开始吐字；
  // pendingRef 提前认领这条会话，避免把开头的提示符/输出丢掉。
  const pendingRef = useRef(null)
  const [status, setStatus] = useState('idle') // idle/connecting/connected
  const [notice, setNotice] = useState('')

  useEffect(() => {
    const term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'Menlo, Consolas, "Courier New", monospace',
      scrollback: 10000,
      theme: { background: '#000000', foreground: '#d4d4d4' },
      convertEol: true,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    termObj.current = term
    fitRef.current = fit

    let timer = 0
    if (termRef.current) {
      term.open(termRef.current)
      timer = window.setTimeout(() => {
        try { fit.fit() } catch { /* 布局未就绪 */ }
        term.write('\r\n\x1b[1;32mMoCenter 远程协助终端\x1b[0m\r\n')
        term.write('点击「连接终端」进入 ' + agentId + ' 的交互 shell。\r\n')
      }, 0)
    }
    const onResize = () => { try { fit.fit() } catch { /* 布局时序 */ } }
    window.addEventListener('resize', onResize)

    const dataDisp = term.onData((data) => {
      const sock = sockRef.current
      if (!sessRef.current) {
        if (data === '\r') term.write('\r\n\x1b[90m（未连接：请先点击「连接终端」）\x1b[0m\r\n')
        return
      }
      sock?.sendInteractive(agentId, sessRef.current, data)
    })
    const resizeDisp = term.onResize(({ rows, cols }) => {
      if (sessRef.current) sockRef.current?.sendInteractive(agentId, sessRef.current, '', { rows, cols })
    })

    return () => {
      window.clearTimeout(timer)
      window.removeEventListener('resize', onResize)
      dataDisp.dispose()
      resizeDisp.dispose()
      try { term.dispose() } catch { /* ignore */ }
      if (sockRef.current) { try { sockRef.current.close() } catch { /* ignore */ } }
      // 关页/切标签也要结束远端 shell，否则访客走了进程还在
      const id = sessRef.current || pendingRef.current
      sessRef.current = null
      pendingRef.current = null
      if (id) ShareApi.commandClose(token, id).catch(() => {})
    }
  }, [agentId, token])

  async function disconnect(silent) {
    const id = sessRef.current || pendingRef.current
    sessRef.current = null
    pendingRef.current = null
    if (sockRef.current) {
      try { sockRef.current.close() } catch { /* ignore */ }
      sockRef.current = null
    }
    setStatus('idle')
    if (id) {
      try { await ShareApi.commandClose(token, id) } catch { /* 链接已失效时忽略 */ }
    }
    if (!silent) termObj.current?.write('\r\n\x1b[1;33m[已断开远程终端]\x1b[0m\r\n')
  }

  const connect = async () => {
    if (!online) { setNotice('目标主机当前离线'); return }
    if (sessRef.current || pendingRef.current) await disconnect(true)
    setStatus('connecting')
    setNotice('')
    const term = termObj.current
    let sock = null
    try {
      sock = new MCSocket({
        token,
        onInteractive: (out) => {
          const cur = sessRef.current || pendingRef.current
          if (!cur || out.command_id !== cur) return
          if (out.type === 'exit') {
            term?.write('\r\n\x1b[1;33m[远端会话已结束 exit=' + (out.exit_code ?? 0) + ']\x1b[0m\r\n')
            sessRef.current = null
            pendingRef.current = null
            setStatus('idle')
            if (sockRef.current) {
              try { sockRef.current.close() } catch { /* ignore */ }
              sockRef.current = null
            }
            return
          }
          if (out.data) term?.write(out.data)
        },
      })
      await new Promise((resolve, reject) => {
        const timer = window.setTimeout(() => reject(new Error('终端通道连接超时')), 8000)
        sock.onStatus = (st) => {
          if (st === 'connected') { window.clearTimeout(timer); resolve() }
        }
        sock.connect()
      })
      sockRef.current = sock
      sock.onStatus = (st) => {
        if (st === 'disconnected' && sessRef.current) setNotice('连接已断开，请重新连接')
      }
      const id = 'share-it-' + Date.now()
      pendingRef.current = id
      const d = await ShareApi.command(token, {
        command_id: id,
        shell_type: '',
        rows: term?.rows || 24,
        cols: term?.cols || 80,
      })
      if (!d?.ok) {
        pendingRef.current = null
        throw new Error(d?.msg || '服务端拒绝建立会话')
      }
      pendingRef.current = null
      sessRef.current = id
      setStatus('connected')
      term?.focus()
    } catch (e) {
      pendingRef.current = null
      try { sock?.close() } catch { /* ignore */ }
      sockRef.current = null
      setStatus('idle')
      setNotice('连接失败：' + (e?.response?.data?.msg || e?.response?.data || e?.message || '未知错误'))
    }
  }

  return (
    <div>
      <div className="terminal-toolbar" style={{ marginTop: 0 }}>
        <span className={`badge ${status === 'connected' ? 'online' : status === 'connecting' ? 'idle' : 'offline'}`}>
          {status === 'connected' ? '会话中' : status === 'connecting' ? '连接中' : '未连接'}
        </span>
        <span className="host-label">{agentId}</span>
        <button className="primary" onClick={connect} disabled={!online || status === 'connecting'}>
          {sessRef.current ? '重连' : '连接终端'}
        </button>
        <button className="secondary" onClick={() => termObj.current?.clear()}>清屏</button>
        <button className="secondary danger" onClick={() => disconnect(false)} disabled={!sessRef.current}>
          断开连接
        </button>
      </div>
      <div className="terminal-wrap">
        <div ref={termRef} className="terminal-body" style={{ height: 'clamp(300px, 58vh, 620px)' }} />
      </div>
      {notice && <div className="notice err" style={{ marginTop: 10 }}>{notice}</div>}
    </div>
  )
}

// ---------------- 远程桌面 ----------------

function VncPanel({ token, info }) {
  const viewerRef = useRef(null)
  const rfbRef = useRef(null)
  const [status, setStatus] = useState('idle') // idle/connecting/connected/disconnected
  const [notice, setNotice] = useState('')
  const [password, setPassword] = useState('')
  const [fetching, setFetching] = useState(false)

  useEffect(() => () => {
    if (rfbRef.current) {
      try { rfbRef.current.disconnect() } catch { /* ignore */ }
      rfbRef.current = null
    }
  }, [])

  const readPassword = async () => {
    setFetching(true)
    setNotice('')
    try {
      const r = await ShareApi.vncPassword(token)
      if (r?.ok && r.password) {
        setPassword(r.password)
        return r.password
      }
      setNotice(r?.msg || '未能读取 VNC 口令，可手工输入后连接')
    } catch (e) {
      setNotice('读取口令失败：' + (e?.response?.data?.msg || e?.message || '网络异常'))
    } finally {
      setFetching(false)
    }
    return ''
  }

  const openViewer = (pw) => {
    if (!viewerRef.current) return
    if (rfbRef.current) {
      try { rfbRef.current.disconnect() } catch { /* ignore */ }
      rfbRef.current = null
    }
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const port = info.vnc_port || 5900
    const url = `${proto}://${location.host}/vnc-ws?agent=${encodeURIComponent(info.agent_id)}&port=${port}`
    const rfb = new RFB(viewerRef.current, url, {
      credentials: { password: pw },
      wsProtocols: ['mocenter', encodeURIComponent(token)],
    })
    rfb.scaleViewport = true
    rfb.showDotCursor = true
    rfbRef.current = rfb
    setStatus('connecting')
    setNotice('')
    rfb.addEventListener('connect', () => { setStatus('connected'); setNotice('') })
    rfb.addEventListener('disconnect', () => setStatus('disconnected'))
    rfb.addEventListener('securityfailure', (e) => {
      setStatus('disconnected')
      setNotice('认证失败：' + (e?.detail?.reason || 'VNC 口令不正确'))
    })
    rfb.addEventListener('credentialsrequired', () => {
      if (pw) rfb.sendCredentials({ password: pw })
    })
  }

  const connect = async () => {
    if (!info.online) { setNotice('目标主机当前离线'); return }
    let pw = password
    if (!pw) pw = await readPassword()
    openViewer(pw)
  }

  const disconnect = () => {
    if (rfbRef.current) {
      try { rfbRef.current.disconnect() } catch { /* ignore */ }
      rfbRef.current = null
    }
    setStatus('disconnected')
  }

  return (
    <div>
      <div className="terminal-toolbar" style={{ marginTop: 0 }}>
        <span className={`badge ${status === 'connected' ? 'online' : status === 'connecting' ? 'idle' : 'offline'}`}>
          {status === 'connected' ? '已连接' : status === 'connecting' ? '连接中…' : '未连接'}
        </span>
        <span className="host-label">{info.agent_id} : {info.vnc_port || 5900}</span>
        <button className="primary" onClick={connect} disabled={!info.online || status === 'connecting'}>连接桌面</button>
        <button className="secondary" onClick={readPassword} disabled={fetching}>
          {fetching ? '读取中…' : '读取口令'}
        </button>
        <button className="secondary" onClick={disconnect} disabled={!rfbRef.current}>断开</button>
      </div>
      <div className="row-inline" style={{ marginBottom: 10, gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <input
          type="password"
          style={{ width: 220 }}
          placeholder="VNC 口令（连接时自动读取，失败可手工输入）"
          value={password}
          autoComplete="off"
          onChange={(e) => setPassword(e.target.value)}
        />
      </div>
      <div className="vnc-wrap">
        <div ref={viewerRef} style={{ width: '100%', height: '100%', minHeight: 360 }} />
        {status !== 'connected' && (
          <div className="vnc-overlay">
            <div>
              <div style={{ fontSize: 15, fontWeight: 600, marginBottom: 6 }}>
                {status === 'connecting' ? '正在连接…' : '未连接'}
              </div>
              <div style={{ fontSize: 12, color: 'var(--muted)' }}>连接成功后可直接在浏览器内操作桌面</div>
            </div>
          </div>
        )}
      </div>
      {notice && <div className="notice err" style={{ marginTop: 10 }}>{notice}</div>}
    </div>
  )
}

// ---------------- 端口转发 ----------------

function PortmapPanel({ token }) {
  const [rules, setRules] = useState([])
  const [pubPort, setPubPort] = useState('')
  const [localPort, setLocalPort] = useState('')
  const [sourceIp, setSourceIp] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const reload = () => {
    ShareApi.rules(token)
      .then((d) => setRules(Array.isArray(d?.rules) ? d.rules : []))
      .catch((e) => setNotice('获取规则失败：' + (e?.response?.data?.msg || e.message)))
  }

  useEffect(() => { reload() }, [token])

  const run = async (op, okText) => {
    setBusy(true)
    setNotice('')
    try {
      const d = await ShareApi.portmapOp(token, op)
      if (!d?.ok) { setNotice(d?.msg || '操作失败'); return false }
      setRules(Array.isArray(d.rules) ? d.rules : [])
      setNotice(okText || d.msg || '操作完成')
      return true
    } catch (e) {
      setNotice('操作失败：' + (e?.response?.data?.msg || e.message))
      return false
    } finally {
      setBusy(false)
    }
  }

  const add = async (e) => {
    e.preventDefault()
    const p = Number(pubPort)
    const l = Number(localPort)
    if (!p || !l) { setNotice('请填写公网端口与内网端口'); return }
    if (p < 1 || p > 65535 || l < 1 || l > 65535) { setNotice('端口范围应为 1-65535'); return }
    const ok = await run({ action: 'create', public_port: p, local_port: l, source_ip: sourceIp.trim() },
      `映射已创建：公网 :${p} → 127.0.0.1:${l}`)
    if (ok) { setPubPort(''); setLocalPort(''); setSourceIp('') }
  }

  return (
    <div>
      <form onSubmit={add}>
        <div className="form-grid">
          <div className="form-row">
            <label>公网端口</label>
            <input type="number" min={1} max={65535} value={pubPort} placeholder="如 8080"
              onChange={(e) => setPubPort(e.target.value)} />
          </div>
          <div className="form-row">
            <label>内网服务端口</label>
            <input type="number" min={1} max={65535} value={localPort} placeholder="如 80"
              onChange={(e) => setLocalPort(e.target.value)} />
          </div>
          <div className="form-row">
            <label>源 IP 白名单（可选）</label>
            <input value={sourceIp} placeholder="留空表示不限制"
              onChange={(e) => setSourceIp(e.target.value)} />
          </div>
        </div>
        <button type="submit" disabled={busy}>{busy ? '提交中…' : '添加映射'}</button>
      </form>
      {notice && <div className={`notice ${/失败|无权|占用/.test(notice) ? 'err' : 'ok'}`} style={{ marginTop: 12 }}>{notice}</div>}

      <div className="table-scroll" style={{ marginTop: 12 }}>
        <table>
          <thead>
            <tr><th>公网端口</th><th>内网端口</th><th>源 IP 白名单</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            {rules.length === 0 && <tr><td colSpan={5} className="table-empty">暂无映射规则</td></tr>}
            {rules.map((r) => (
              <tr key={r.public_port || r.id}>
                <td>{r.public_port}</td>
                <td>{r.local_port}</td>
                <td>{r.source_ip_whitelist || r.source_ip || '不限'}</td>
                <td>
                  <span className={`badge ${r.enabled ? 'online' : 'offline'}`}>{r.enabled ? '已启用' : '已禁用'}</span>
                </td>
                <td>
                  <span style={{ display: 'inline-flex', gap: 6 }}>
                    <button className="secondary btn-sm" disabled={busy}
                      onClick={() => run({ action: r.enabled ? 'disable' : 'enable', public_port: r.public_port, local_port: r.local_port },
                        r.enabled ? '映射已停用' : '映射已启用')}>
                      {r.enabled ? '停用' : '启用'}
                    </button>
                    <button className="secondary danger btn-sm" disabled={busy}
                      onClick={() => { if (window.confirm(`删除公网 :${r.public_port} 的映射？`)) run({ action: 'delete', public_port: r.public_port }, '映射已删除') }}>
                      删除
                    </button>
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
