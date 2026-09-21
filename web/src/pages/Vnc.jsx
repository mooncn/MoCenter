import React, { useRef, useState, useEffect, useMemo } from 'react'
import RFB from '@novnc/novnc'
import { Api } from '../api/api.js'
import { createVncPointer, WHEEL_STEP_DELTA } from '../utils/vncPointer.js'

// 远端光标显示尺寸档位：相对「按视口等比缩放」基准的倍数。
// 基准 = 显示宽度 / 远端分辨率宽度，这样光标与缩放过后的桌面比例一致。
const CURSOR_SIZES = [
  { label: '小', factor: 0.6 },
  { label: '标准', factor: 1 },
  { label: '大', factor: 1.5 },
]

// VNC 远程桌面：通过服务端隧道连接目标主机内网 VNC，浏览器侧由 noVNC 渲染桌面。
export default function VncPage({ agents }) {
  const viewerRef = useRef(null)
  const wrapRef = useRef(null)
  const rfbRef = useRef(null)
  const hostRef = useRef('')
  // 虚拟鼠标的指针位置/手势状态由 vncPointer 控制器持有（见下方 vp）
  // 是否因认证失败而断开（用于避免通用断开文案覆盖更精确的提示）
  const authFailedRef = useRef(false)

  const [host, setHost] = useState('')
  const [port, setPort] = useState(5900)
  // 安装模式：interactive=交互会话模式（需已登录且未锁屏）；
  // service=服务模式（UltraVNC 系统服务，锁屏/登录界面亦可查看）
  const [mode, setMode] = useState('interactive')
  const [status, setStatus] = useState('idle') // idle/connecting/connected/disconnected
  const [notice, setNotice] = useState('')
  const [vncUp, setVncUp] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [password, setPassword] = useState('')
  const [fullscreen, setFullscreen] = useState(false)
  // 连接工具栏（右下角）展开状态：窄屏默认收起，避免遮挡桌面
  const [toolsOpen, setToolsOpen] = useState(false)
  // 远端光标显示尺寸档位（索引进 CURSOR_SIZES，相对「按视口等比」基准的倍数）
  const [cursorIdx, setCursorIdx] = useState(1)
  // 虚拟鼠标面板（移动端触控板 + 按键）
  const [mouseOpen, setMouseOpen] = useState(false)
  // 虚拟鼠标是否按住左键（用于拖拽窗口/选中文本）
  const [holdLeft, setHoldLeft] = useState(false)

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
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    // Token 改走 Sec-WebSocket-Protocol 子协议头（见下），不再拼在 URL 上
    return `${proto}://${location.host}/vnc-ws?agent=${encodeURIComponent(hostRef.current)}&port=${port}`
  }

  const openViewer = () => {
    if (!viewerRef.current) return
    if (rfbRef.current) { try { rfbRef.current.disconnect() } catch {} rfbRef.current = null }

    const token = localStorage.getItem('mc_token') || ''
    // noVNC 支持 wsProtocols：借子协议头把 Token 交给服务端，避免 Token 进 URL/日志
    const rfb = new RFB(viewerRef.current, buildWsUrl(), {
      credentials: { password },
      wsProtocols: ['mocenter', encodeURIComponent(token)],
    })
    rfb.scaleViewport = true
    rfb.resizeSession = false
    // 兜底：若服务端未下发光标形状（UltraVNC 需 ForceCursorShape=1 才会下发），
    // 至少显示一个圆点，避免完全看不到指针。收到真实光标时该圆点会自动隐藏。
    rfb.showDotCursor = true
    rfbRef.current = rfb

    setStatus('connecting')
    setNotice('正在连接 VNC 桌面，等待 RFB 握手/口令认证...')
    rfb.addEventListener('connect', () => { authFailedRef.current = false; setStatus('connected'); setNotice('') })
    rfb.addEventListener('disconnect', (e) => {
      setStatus('disconnected')
      // 认证失败已有更精确的提示，不要用通用文案覆盖它（否则口令错误会被显示成"连接已断开"）
      if (e?.detail?.clean === false && !authFailedRef.current) {
        setNotice('连接已断开。若口令有误，请核对后重试；若未安装 VNC，请先一键安装。')
      }
    })
    rfb.addEventListener('securityfailure', (e) => {
      authFailedRef.current = true
      setNotice('认证失败：' + (e?.detail?.reason || 'VNC 口令不正确或需要口令'))
    })
    // 服务端在握手中索要口令时补交
    rfb.addEventListener('credentialsrequired', () => {
      rfb.sendCredentials({ password })
    })
  }

  // ---- 远端光标显示尺寸 ----
  //
  // noVNC 在触屏设备上会走 Cursor 的 fallback 分支：把光标画在 document.body 下
  // 一个 position:fixed 的独立 canvas 上，并按「远端原始像素」绘制
  // （见 noVNC core/util/cursor.js）。它不随 scaleViewport 缩放，因此当桌面被缩小
  // 显示时，光标仍按原尺寸绘制、看起来明显偏大。
  // 这里按「显示宽度 / 远端分辨率宽度」等比缩放该 canvas，使光标与桌面比例一致。
  const screenCanvas = () => (viewerRef.current ? viewerRef.current.querySelector('canvas') : null)

  const findCursorCanvas = () => {
    // fallback 光标 canvas 直接挂在 body 下，是本页面唯一的 body > canvas
    for (const el of Array.from(document.body.children)) {
      if (el.tagName === 'CANVAS') return el
    }
    return null
  }

  const applyCursorScale = () => {
    const cur = findCursorCanvas()
    if (!cur) return // 非 fallback 模式（桌面浏览器）由系统绘制光标，无需处理
    const screen = screenCanvas()
    let base = 1
    if (screen && screen.width > 0 && screen.clientWidth > 0) {
      base = screen.clientWidth / screen.width
    }
    const k = Math.max(0.25, Math.min(4, base * CURSOR_SIZES[cursorIdx].factor))
    // 以左上角为原点缩放：标准箭头光标的热点即为 (0,0)，误差可忽略
    cur.style.transformOrigin = '0 0'
    cur.style.transform = `scale(${k})`
  }

  const cycleCursorSize = () => setCursorIdx((i) => (i + 1) % CURSOR_SIZES.length)

  // ---- 虚拟鼠标（移动端）----
  //
  // 手势状态与「该发出什么鼠标事件」全部交给 vncPointer 控制器（纯逻辑、可单测）；
  // 这里只做两件事：把 DOM 指针事件转发进去，以及把控制器给出的画布内坐标换算成
  // client 坐标、派发给 noVNC 的 canvas —— 由 noVNC 自己的 _handleMouse 完成
  // client→远端 坐标换算并绘制光标，因此「画出的光标」与「远端真实指针」始终一致。
  const emitMouse = (evt) => {
    const cv = screenCanvas()
    if (!cv) return
    const r = cv.getBoundingClientRect()
    const clientX = r.left + evt.x
    const clientY = r.top + evt.y
    if (evt.type === 'wheel') {
      cv.dispatchEvent(new WheelEvent('wheel', {
        clientX, clientY, deltaY: evt.deltaY, deltaMode: 0, bubbles: true, cancelable: true,
      }))
      return
    }
    cv.dispatchEvent(new MouseEvent(evt.type, {
      clientX, clientY,
      buttons: evt.buttons, button: evt.buttons === 2 ? 2 : 0,
      bubbles: true, cancelable: true,
    }))
  }

  const vp = useMemo(() => createVncPointer({
    getRect: () => {
      const cv = screenCanvas()
      if (!cv) return null
      const r = cv.getBoundingClientRect()
      return { width: r.width, height: r.height }
    },
    emit: emitMouse,
    onHoldChange: setHoldLeft,
  }), [])

  // 触控板：单指拖动 = 相对移动；移动量很小的抬手 = 轻点（左键单击）。
  // 指针捕获让手指移出触控板范围后仍能继续收到事件。
  const onTrackDown = (e) => {
    if (e.currentTarget.setPointerCapture) {
      try { e.currentTarget.setPointerCapture(e.pointerId) } catch { /* ignore */ }
    }
    vp.gestureStart(e.clientX, e.clientY)
  }

  const onTrackMove = (e) => vp.gestureMove(e.clientX, e.clientY)

  const onTrackUp = () => vp.gestureEnd()

  // 手势被系统取消（iOS 边缘返回、下拉通知、来电等）时不能当作轻点，
  // 否则会在远端凭空多出一次左键点击。
  const onTrackCancel = () => vp.gestureCancel()

  // 面板收起 / 连接断开时收尾「按住的左键」：否则远端会一直停在按下状态
  // （表现为拖选一切），而面板已收起、连释放按钮都点不到。
  useEffect(() => {
    if (holdLeft && (!mouseOpen || status !== 'connected')) vp.releaseHold()
  }, [holdLeft, mouseOpen, status, vp])

  // 组件卸载时同样收尾（手势 + 按住状态）
  useEffect(() => () => vp.detach(), [vp])

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
    // 先收尾虚拟鼠标按住的左键，再断开 —— 反过来就没有通道把它送到远端了
    try { vp.detach() } catch { /* ignore */ }
    if (rfbRef.current) { try { rfbRef.current.disconnect() } catch {} rfbRef.current = null }
    setStatus('disconnected')
    setNotice('')
  }

  // 一键安装/启动 VNC；成功后自动回填服务端下发的口令。
  // 若安装较慢导致本次请求超时，轮询 /vnc/status 补取结果，再按需向 /vnc/password 取口令。
  const install = async () => {
    if (!host) { setNotice('请先选择目标主机'); return }
    setInstalling(true)
    setNotice('正在下发安装/启动指令，请稍候...')
    // 状态接口只说"有口令可用"，取的动作必须显式做一次（服务端据此留痕）
    const takePassword = async () => {
      try {
        const r = await Api.vncPassword(host)
        if (r && r.ok && r.password) {
          setPassword(r.password)
          sessionStorage.setItem(pwKey(), r.password)
          return true
        }
      } catch { /* 取不到就保留手工输入 */ }
      return false
    }
    const apply = async (r) => {
      const modeLabel = r.mode === 'service' ? '服务模式' : r.mode === 'interactive' ? '交互会话模式' : ''
      let tail = ''
      if (r.password) {
        setPassword(r.password); sessionStorage.setItem(pwKey(), r.password)
      } else if (r.has_password && await takePassword()) {
        tail = '（口令已按需获取）'
      }
      setNotice((r.ok ? '✅ ' : '❌ ') + (modeLabel ? `[${modeLabel}] ` : '') + (r.msg || r.message || '执行完成') + tail)
      setVncUp(!!r.ok)
    }
    // 轮询 /vnc/status 补取安装结果（口令不在其中），最多等 5 分钟
    const pollStatus = async () => {
      for (let i = 0; i < 60; i++) {
        await new Promise((res) => setTimeout(res, 3000))
        try {
          const r = await Api.vncStatus(host)
          if (r && (r.has_password || r.ok)) return r
          // 安装明确失败时也提前退出
          if (r && r.ok === false && r.msg && /失败|错误|不支持/.test(r.msg)) return r
        } catch { /* retry */ }
      }
      return null
    }
    try {
      let r = await Api.vncInstall({ agent_id: host, port, mode })
      // 请求成功但未带回口令时，轮询补取最终结果
      if (!r.password) {
        r = await pollStatus() || r
      }
      await apply(r)
    } catch (ex) {
      // 请求超时或网络错误：安装可能仍在后台进行，轮询补取结果
      setNotice('请求超时或网络异常，正在轮询安装结果...')
      const r = await pollStatus()
      if (r) {
        await apply(r)
      } else {
        setNotice('下发失败：' + (ex.response?.data?.msg || ex.message) + '。可稍后点击"一键安装VNC"重试，或以管理员身份运行 Agent。')
      }
    } finally {
      setInstalling(false)
    }
  }

  // 连接成功 / 尺寸档位变化 / 窗口缩放时，重新按视口比例校准远端光标大小。
  // 用 rAF 延迟一帧，确保 noVNC 已把光标 canvas 挂到 body 上。
  useEffect(() => {
    if (status !== 'connected') return
    const id = requestAnimationFrame(applyCursorScale)
    const onResize = () => applyCursorScale()
    window.addEventListener('resize', onResize)
    return () => { cancelAnimationFrame(id); window.removeEventListener('resize', onResize) }
  }, [status, cursorIdx])

  // 打开虚拟鼠标面板时把指针归到桌面中心：
  // 远端指针的真实位置客户端无从得知，先发一次移动建立共同基准，
  // 之后的相对移动才不会出现「一拖就跳」。
  useEffect(() => {
    if (!mouseOpen || status !== 'connected') return
    vp.syncToCenter()
  }, [mouseOpen, status, vp])

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
            <label>连接模式</label>
            <select value={mode} onChange={(e) => setMode(e.target.value)} style={{ width: 280 }}
              title="决定连接时能看到什么。服务模式以系统服务运行，可抓取 Winlogon 安全桌面，锁屏/登录界面也能看到画面。切换模式后需重新点击「一键安装VNC」生效。">
              <option value="interactive">交互会话模式（需已登录且未锁屏）</option>
              <option value="service">服务模式（支持锁屏，Windows 用 UltraVNC）</option>
            </select>
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
                <button className="vnc-fab-item" onClick={() => { const r = rfbRef.current; if (r) r.scaleViewport = !r.scaleViewport; setTimeout(applyCursorScale, 0) }} title="切换自适应缩放 / 原始分辨率">
                  ⇱ 自适应缩放
                </button>
                <button className="vnc-fab-item" onClick={() => { const r = rfbRef.current; if (r) r.sendCtrlAltDel() }} title="发送 Ctrl+Alt+Del">
                  ⌨ Ctrl+Alt+Del
                </button>
                <button className="vnc-fab-item" onClick={() => setMouseOpen((v) => !v)} title="移动端虚拟鼠标：触控板移动 + 按键点击">
                  🖱 {mouseOpen ? '隐藏虚拟鼠标' : '虚拟鼠标'}
                </button>
                <button className="vnc-fab-item" onClick={cycleCursorSize} title="调整远端光标的显示大小（小 / 标准 / 大）">
                  ◉ 光标大小：{CURSOR_SIZES[cursorIdx].label}
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

        {/* 移动端虚拟鼠标：触控板做相对移动，按钮做点击/滚轮/按住拖拽。
            刻意放在画布**下方**（而不是浮在桌面之上）：覆盖式会把远端桌面最底部一条
            挡住且点不到（任务栏就在那条上）。 */}
        {mouseOpen && status === 'connected' && (
          <div className="vnc-mouse">
            <div
              className="vnc-mouse-track"
              onPointerDown={onTrackDown}
              onPointerMove={onTrackMove}
              onPointerUp={onTrackUp}
              onPointerCancel={onTrackCancel}
            >
              <span>触控板：拖动移动指针 · 轻点 = 左键</span>
            </div>
            <div className="vnc-mouse-btns">
              <button onClick={() => vp.click(1)}>左键</button>
              <button onClick={() => vp.click(2)}>右键</button>
              <button onClick={() => vp.doubleClick()}>双击</button>
              <button onClick={() => vp.wheel(-WHEEL_STEP_DELTA)}>滚轮↑</button>
              <button onClick={() => vp.wheel(WHEEL_STEP_DELTA)}>滚轮↓</button>
              <button className={holdLeft ? 'on' : ''} onClick={() => vp.toggleHold()}>
                {holdLeft ? '松开左键' : '按住左键'}
              </button>
            </div>
          </div>
        )}
        {notice && <div className="notice" style={{ marginTop: 10 }}>{notice}</div>}
      </div>
    </div>
  )
}