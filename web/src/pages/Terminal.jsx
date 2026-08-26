import React, { useEffect, useRef, useState, useContext } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Api } from '../api/api.js'
import { SocketContext } from '../App.jsx'

// 远程命令执行界面：基于 xterm.js 的终端模拟组件
export default function TerminalPage({ agents }) {
  const termRef = useRef(null)
  const fitAddonRef = useRef(null)
  const terminalRef = useRef(null)
  const socket = useContext(SocketContext)

  const [host, setHost] = useState('')
  const [shell, setShell] = useState('auto')
  const [cmdId, setCmdId] = useState('')
  const [status, setStatus] = useState('idle') // idle/connecting/executing
  const [notice, setNotice] = useState('')

  const onlineHosts = (agents || []).filter((a) => a.online)

  // 初始化 xterm
  useEffect(() => {
    const term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'Menlo, Consolas, "Courier New", monospace',
      scrollback: 10000,
      theme: { background: '#000000', foreground: '#d4d4d4' },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    terminalRef.current = term

    if (termRef.current) {
      term.open(termRef.current)
      fit.fit()
      term.writeln('\x1b[1;32mMoCenter 远程终端\x1b[0m')
      term.writeln('选择一个在线主机，输入命令后回车执行。输入 /clear 清屏。\r\n')
    }

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    // 接收 WebSocket 推送的命令输出与交互输出
    if (socket) {
      socket.handlers.onCommandResp = (resp) => {
        if (resp.command_id !== cmdIdRef.current) return
        if (resp.stdout) term.write(resp.stdout)
        if (resp.stderr) term.write('\x1b[1;31m' + resp.stderr + '\x1b[0m')
        term.write('\r\n\x1b[1;33m[exit=' + (resp.exit_code ?? resp.status) + ']\x1b[0m\r\n')
        setStatus('idle')
      }
      socket.handlers.onInteractive = (out) => {
        term.write(out.data || '')
      }
    }

    return () => {
      window.removeEventListener('resize', onResize)
      term.dispose()
    }
  }, [socket])

  const cmdIdRef = useRef('')
  const hostRef = useRef('')

  useEffect(() => {
    cmdIdRef.current = cmdId
  }, [cmdId])
  useEffect(() => {
    hostRef.current = host
  }, [host])

  // 处理回车执行命令
  useEffect(() => {
    if (!terminalRef.current) return
    const term = terminalRef.current
    let line = ''
    const onData = (data) => {
      // 回车执行
      if (data === '\r') {
        term.write('\r\n')
        if (line.trim() === '/clear') {
          term.clear()
          line = ''
          return
        }
        if (line.trim() === '') { line = ''; return }
        execute(line)
        line = ''
        return
      }
      if (data === '\u007f') { // backspace
        if (line.length > 0) {
          line = line.slice(0, -1)
          term.write('\b \b')
        }
        return
      }
      line += data
      term.write(data)
    }
    // xterm.js v5: onData 返回 disposable，清理时调用 dispose()
    const disposable = term.onData(onData)

    async function execute(cmd) {
      if (!hostRef.current) {
        term.write('\x1b[1;31m请先选择目标主机\x1b[0m\r\n')
        return
      }
      const id = 'cmd-' + Date.now()
      cmdIdRef.current = id
      setCmdId(id)
      setStatus('executing')
      try {
        await Api.command({
          command_id: id,
          agent_id: hostRef.current,
          command: cmd,
          shell_type: shell === 'auto' ? '' : shell,
          timeout: 30,
        })
      } catch (ex) {
        term.write('\x1b[1;31m命令下发失败: ' + (ex.response?.data?.msg || ex.message) + '\x1b[0m\r\n')
        setStatus('idle')
      }
    }
    return () => disposable.dispose()
  }, [shell])

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>远程终端</h2>
          <div className="sub">选择主机并在终端中执行远程命令</div>
        </div>
      </div>

      <div className="card">
        <div className="form-grid">
          <div className="form-row">
            <label>目标主机</label>
            <select value={host} onChange={(e) => setHost(e.target.value)}>
              <option value="">请选择在线主机...</option>
              {onlineHosts.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>{a.hostname} ({a.agent_id})</option>
              ))}
            </select>
          </div>
          <div className="form-row">
            <label>Shell 环境</label>
            <select value={shell} onChange={(e) => setShell(e.target.value)}>
              <option value="auto">自动推断</option>
              <option value="bash">bash</option>
              <option value="sh">sh</option>
              <option value="cmd">cmd</option>
              <option value="powershell">powershell</option>
            </select>
          </div>
        </div>
        <div className="terminal-toolbar">
          <span className={`badge ${status === 'executing' ? 'idle' : status === 'idle' ? 'online' : 'offline'}`}>
            {status === 'executing' ? '执行中' : '就绪'}
          </span>
          <span className="host-label">{host ? onlineHosts.find((h) => h.agent_id === host)?.hostname : '未选择主机'}</span>
          <button className="secondary" onClick={() => terminalRef.current && terminalRef.current.clear()}>清屏</button>
        </div>
        <div className="terminal-wrap">
          <div ref={termRef} style={{ height: 420 }} />
        </div>
        {notice && <div className="notice err">{notice}</div>}
      </div>
    </div>
  )
}
