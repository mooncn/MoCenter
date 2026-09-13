import React, { useEffect, useRef, useState, useContext } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Api } from '../api/api.js'
import { SocketContext } from '../App.jsx'

// 远程终端：基于 xterm.js + 服务端交互会话（PTY/ConPTY）。
// 连接后在目标主机保持一个持久 shell，可连续执行多条命令，
// 键入即转发（含 Ctrl+C/退格/方向键等控制键），断开时结束远端进程。
export default function TerminalPage({ agents }) {
  const termRef = useRef(null)
  const fitAddonRef = useRef(null)
  const terminalRef = useRef(null)
  const socket = useContext(SocketContext)

  const [host, setHost] = useState('')
  const [shell, setShell] = useState('auto')
  const [status, setStatus] = useState('idle') // idle/connecting/connected
  const [notice, setNotice] = useState('')

  const onlineHosts = (agents || []).filter((a) => a.online)

  // 会话状态（ref 供事件回调读取，state 供渲染）
  const sessRef = useRef(null) // { id, agent }
  const [sessId, setSessId] = useState('')

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
    terminalRef.current = term
    fitAddonRef.current = fit

    let fitTimer = 0
    if (termRef.current) {
      term.open(termRef.current)
      // xterm 渲染器需在 DOM 完成布局后才能读取容器尺寸；
      // 若立即 fit，容器尚未就绪时会抛 Viewport dimensions 异常并导致整页崩溃，
      // 故延迟到下一帧执行 fit 与横幅写入。
      fitTimer = window.setTimeout(() => {
        try { fit.fit() } catch { /* 布局时序，忽略 */ }
        term.write('\r\n\x1b[1;32mMoCenter 远程终端\x1b[0m\r\n')
        term.write('选择主机后点击「连接」，即进入持久交互会话（支持连续执行命令、Ctrl+C）。\r\n')
      }, 0)
    }

    const onResize = () => { try { fit.fit() } catch { /* 布局时序，忽略 */ } }
    window.addEventListener('resize', onResize)
    // 移动设备旋转屏幕后尺寸变化，需要重新 fit
    window.addEventListener('orientationchange', onResize)

    // 接收交互输出：PTY 回显 + 命令输出均由此到达
    if (socket) {
      socket.handlers.onInteractive = (out) => {
        if (!sessRef.current || out.command_id !== sessRef.current.id) return
        const term = terminalRef.current
        if (!term) return
        if (out.type === 'exit') {
          term.write('\r\n\x1b[1;33m[会话已结束 exit=' + (out.exit_code ?? 0) + ']\x1b[0m\r\n')
          sessRef.current = null
          setSessId('')
          setStatus('idle')
          return
        }
        if (out.data) term.write(out.data)
      }
    }

    return () => {
      window.clearTimeout(fitTimer)
      window.removeEventListener('resize', onResize)
      window.removeEventListener('orientationchange', onResize)
      term.dispose()
    }
  }, [socket])

  // 键入转发：会话激活时，所有按键（含控制字符）直接送远端 PTY
  useEffect(() => {
    if (!terminalRef.current || !socket) return
    const term = terminalRef.current
    const disposable = term.onData((data) => {
      if (!sessRef.current) {
        if (data === '\r') term.write('\r\n\x1b[90m（未连接：请先选择主机并点击「连接」）\x1b[0m\r\n')
        return
      }
      socket.sendInteractive(sessRef.current.agent, sessRef.current.id, data)
    })
    // 终端尺寸变化同步到远端 PTY
    const resizeDisposable = term.onResize(({ rows, cols }) => {
      if (sessRef.current) {
        socket.sendInteractive(sessRef.current.agent, sessRef.current.id, '', { rows, cols })
      }
    })
    return () => { disposable.dispose(); resizeDisposable.dispose() }
  }, [socket])

  // 切换主机/shell 时断开旧会话
  useEffect(() => {
    return () => { /* 卸载由全局清理 */ }
  }, [])

  const disconnectRemote = async (silent = false) => {
    const s = sessRef.current
    sessRef.current = null
    setSessId('')
    setStatus('idle')
    if (!s) return
    try { await Api.commandClose({ agent_id: s.agent, command_id: s.id, kill_all: false }) } catch { /* ignore */ }
    if (!silent && terminalRef.current) {
      terminalRef.current.write('\r\n\x1b[1;33m[已断开，远端会话与相关进程已结束]\x1b[0m\r\n')
    }
  }

  const connect = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
    if (sessRef.current) await disconnectRemote(true)
    const term = terminalRef.current
    const id = 'it-' + Date.now()
    setStatus('connecting')
    setNotice('')
    try {
      await Api.command({
        command_id: id,
        agent_id: host,
        command: '',
        shell_type: shell === 'auto' ? '' : shell,
        interactive: true,
        rows: term ? term.rows : 24,
        cols: term ? term.cols : 80,
        timeout: 0,
      })
      sessRef.current = { id, agent: host }
      setSessId(id)
      setStatus('connected')
      term?.focus()
    } catch (ex) {
      setStatus('idle')
      setNotice('连接失败: ' + (ex.response?.data?.msg || ex.message))
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>远程终端</h2>
          <div className="sub">持久交互会话 · 支持连续命令与控制键</div>
        </div>
      </div>

      <div className="card">
        <div className="form-grid">
          <div className="form-row">
            <label>目标主机</label>
            <select value={host} onChange={(e) => setHost(e.target.value)} disabled={status === 'connecting'}>
              <option value="">请选择在线主机...</option>
              {onlineHosts.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>{a.hostname} ({a.agent_id})</option>
              ))}
            </select>
          </div>
          <div className="form-row">
            <label>Shell 环境</label>
            <select value={shell} onChange={(e) => setShell(e.target.value)} disabled={status === 'connecting'}>
              <option value="auto">自动推断</option>
              <option value="bash">bash</option>
              <option value="sh">sh</option>
              <option value="cmd">cmd</option>
              <option value="powershell">powershell</option>
            </select>
          </div>
        </div>
        <div className="terminal-toolbar">
          <span className={`badge ${status === 'connected' ? 'online' : status === 'connecting' ? 'idle' : 'offline'}`}>
            {status === 'connected' ? '会话中' : status === 'connecting' ? '连接中' : '未连接'}
          </span>
          <span className="host-label">
            {sessId
              ? (onlineHosts.find((h) => h.agent_id === sessRef.current?.agent)?.hostname || sessRef.current?.agent)
              : (host ? onlineHosts.find((h) => h.agent_id === host)?.hostname + '（未连接）' : '未选择主机')}
          </span>
          <button className="primary" onClick={connect} disabled={!host || status === 'connecting'}>
            {sessId ? '重连' : '连接'}
          </button>
          <button className="secondary" onClick={() => terminalRef.current && terminalRef.current.clear()}>清屏</button>
          <button className="secondary danger" onClick={() => disconnectRemote()} disabled={!sessId}
            title="断开会话并结束远端相关进程">
            断开连接
          </button>
        </div>
        <div className="terminal-wrap">
          <div ref={termRef} className="terminal-body" style={{ height: 'clamp(280px, 55vh, 560px)' }} />
        </div>
        {notice && <div className="notice err">{notice}</div>}
      </div>
    </div>
  )
}
