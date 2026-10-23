import React, { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Api } from '../api/api.js'
import Account from './Account.jsx'

// 系统配置：账号与安全 + 登录会话 + 文件编辑上限 + 两个监听地址。
// 入口统一为一个页面，各分区用可折叠（toggle）卡片展示，避免把高风险配置
// 和日常改密码混在一屏。
const TTL_OPTIONS = [
  { value: '1h', label: '1 小时' },
  { value: '12h', label: '12 小时' },
  { value: '24h', label: '24 小时' },
  { value: '168h', label: '7 天' },
  { value: '360h', label: '15 天' },
  { value: '720h', label: '30 天' },
]

function Section({ id, title, sub, open, onToggle, children }) {
  return (
    <details
      className="card cfg-section"
      open={open}
      onToggle={(e) => onToggle(id, e.currentTarget.open)}
    >
      <summary>
        <div className="cfg-summary-text">
          <strong>{title}</strong>
          {sub && <span className="cell-sub">{sub}</span>}
        </div>
        <span className="cfg-chevron" aria-hidden="true">▾</span>
      </summary>
      <div className="cfg-body">{children}</div>
    </details>
  )
}

function errText(e) {
  return e?.response?.data?.msg || e?.message || '操作失败'
}

export default function SystemConfig() {
  const [params] = useSearchParams()
  const initial = params.get('section') || 'account'
  const [open, setOpen] = useState({
    account: initial === 'account',
    session: initial === 'session',
    files: initial === 'files',
    agent: initial === 'agent',
    http: initial === 'http',
  })
  const toggle = (id, v) => setOpen((o) => (o[id] === v ? o : { ...o, [id]: v }))

  const [cfg, setCfg] = useState(null)
  const [loadErr, setLoadErr] = useState('')

  // 会话保持时长
  const [ttl, setTtl] = useState('12h')
  const [ttlMsg, setTtlMsg] = useState(null)
  const [ttlBusy, setTtlBusy] = useState(false)

  // 文件在线预览/编辑上限
  const [editMB, setEditMB] = useState('10')
  const [listPageSize, setListPageSize] = useState('100')
  const [editMsg, setEditMsg] = useState(null)
  const [editBusy, setEditBusy] = useState(false)

  // Agent 监听
  const [agentHost, setAgentHost] = useState('')
  const [agentPort, setAgentPort] = useState('9000')
  const [agentPw, setAgentPw] = useState('')
  const [agentConfirm, setAgentConfirm] = useState(false)
  const [agentMsg, setAgentMsg] = useState(null)
  const [agentBusy, setAgentBusy] = useState(false)

  // 后台监听
  const [httpHost, setHttpHost] = useState('')
  const [httpPort, setHttpPort] = useState('8080')
  const [httpPw, setHttpPw] = useState('')
  const [httpConfirm, setHttpConfirm] = useState(false)
  const [httpMsg, setHttpMsg] = useState(null)
  const [httpBusy, setHttpBusy] = useState(false)

  const hydrate = (d) => {
    setCfg(d)
    setTtl(d.token_ttl || '12h')
    setEditMB(String(d.file_edit_max_mb || 10))
    setListPageSize(String(d.file_list_page_size || 100))
    // 默认主机按需求取"本机外网 IP"；通配监听时后端返回的 host 为空。
    setAgentHost(d.agent_listen?.host || d.public_ip || '')
    setAgentPort(String(d.agent_listen?.port || 9000))
    setHttpHost(d.http_listen?.host || d.public_ip || '')
    setHttpPort(String(d.http_listen?.port || 8080))
  }

  const load = () => {
    Api.systemConfig()
      .then((d) => {
        if (d?.ok === false) throw new Error(d.msg || '读取系统配置失败')
        setLoadErr('')
        hydrate(d)
      })
      .catch((e) => setLoadErr(errText(e)))
  }

  useEffect(() => { load() }, [])

  const saveTTL = async () => {
    setTtlBusy(true)
    setTtlMsg(null)
    try {
      const d = await Api.setSessionTTL(ttl)
      setTtlMsg({ ok: !!d.ok, text: d.msg || (d.ok ? '已保存' : '保存失败') })
      if (d.ok) load()
    } catch (e) {
      setTtlMsg({ ok: false, text: errText(e) })
    } finally {
      setTtlBusy(false)
    }
  }

  const saveFileCfg = async () => {
    setEditMsg(null)
    const mb = Number(editMB)
    const ps = Number(listPageSize)
    if (!Number.isInteger(mb) || mb < 1 || mb > 64) {
      setEditMsg({ ok: false, text: '编辑上限需在 1–64 MiB 之间' })
      return
    }
    if (!Number.isInteger(ps) || ps < 20 || ps > 1000) {
      setEditMsg({ ok: false, text: '每页条数需在 20–1000 之间' })
      return
    }
    setEditBusy(true)
    try {
      const a = await Api.setFileEditLimit(mb)
      if (!a.ok) { setEditMsg({ ok: false, text: a.msg || '保存失败' }); return }
      const b = await Api.setFileListPageSize(ps)
      setEditMsg({ ok: !!b.ok, text: b.ok ? '已保存' : (b.msg || '保存失败') })
      if (b.ok) load()
    } catch (e) {
      setEditMsg({ ok: false, text: errText(e) })
    } finally {
      setEditBusy(false)
    }
  }

  const saveAgentListen = async () => {
    setAgentMsg(null)
    if (!agentPw) { setAgentMsg({ ok: false, text: '请填写当前登录密码' }); return }
    if (!agentConfirm) { setAgentMsg({ ok: false, text: '请先勾选风险确认' }); return }
    setAgentBusy(true)
    try {
      const d = await Api.setAgentListen({
        host: agentHost.trim(),
        port: Number(agentPort),
        password: agentPw,
        confirm: true,
      })
      setAgentMsg({ ok: !!d.ok, text: d.msg || (d.ok ? '已应用' : '保存失败') })
      if (d.ok) {
        setAgentPw('')
        setAgentConfirm(false)
        load()
      }
    } catch (e) {
      setAgentMsg({ ok: false, text: errText(e) })
    } finally {
      setAgentBusy(false)
    }
  }

  const saveHTTPListen = async () => {
    setHttpMsg(null)
    if (!httpPw) { setHttpMsg({ ok: false, text: '请填写当前登录密码' }); return }
    if (!httpConfirm) { setHttpMsg({ ok: false, text: '请先勾选风险确认' }); return }
    setHttpBusy(true)
    const currentPort = window.location.port ||
      (window.location.protocol === 'https:' ? '443' : '80')
    const portChanged = String(httpPort) !== String(currentPort)
    // 必须在旧地址仍然可用时申请交接码：切端口后旧 origin 会关闭，
    // 新 origin 没有 localStorage Token。
    let handoff = ''
    if (portChanged) {
      try {
        const h = await Api.createHandoff()
        if (h?.ok && h.code) handoff = h.code
      } catch { /* 交接失败不阻断切换，最多落新地址后重新登录 */ }
    }
    try {
      const d = await Api.setHTTPListen({
        host: httpHost.trim(),
        port: Number(httpPort),
        password: httpPw,
        confirm: true,
      })
      setHttpMsg({ ok: !!d.ok, text: d.msg || (d.ok ? '已应用' : '保存失败') })
      if (d.ok) {
        setHttpPw('')
        setHttpConfirm(false)
        // 不再 load()：旧监听可能已经在关闭，继续请求旧地址只会报错。
        // 端口发生变化时直接跳到「当前访问地址 + 新端口」，保留系统配置页与分区。
        if (portChanged) {
          const target = new URL(window.location.href)
          target.port = String(httpPort)
          target.pathname = '/admin/system'
          target.search = '?section=http'
          if (handoff) target.searchParams.set('handoff', handoff)
          setHttpMsg({ ok: true, text: `${d.msg || '已切换'} 正在跳转到新地址…` })
          setTimeout(() => { window.location.href = target.toString() }, 900)
        }
      }
    } catch (e) {
      setHttpMsg({ ok: false, text: errText(e) })
    } finally {
      setHttpBusy(false)
    }
  }

  const httpNewUrl = (() => {
    const wildcard = ['', '0.0.0.0', '::'].includes(httpHost.trim())
    const host = wildcard ? window.location.hostname : httpHost.trim()
    const port = String(httpPort || '').trim()
    if (!host || !port) return ''
    const h = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
    return `${window.location.protocol}//${h}:${port}/admin`
  })()

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>系统配置</h2>
          <div className="sub">账号安全、登录会话与网络监听。高风险项修改前需验证当前密码。</div>
        </div>
      </div>

      {loadErr && <div className="notice err" style={{ marginBottom: 12 }}>{loadErr}</div>}

      <Section
        id="account" title="账号与安全" sub="修改登录密码 / OTP 动态验证码"
        open={open.account} onToggle={toggle}
      >
        <Account embedded />
      </Section>

      <Section
        id="session" title="登录会话" sub="单次登录保持时长，过期自动注销"
        open={open.session} onToggle={toggle}
      >
        <div className="cfg-grid">
          <label>
            保持时长
            <select value={ttl} onChange={(e) => setTtl(e.target.value)}>
              {TTL_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
              {!TTL_OPTIONS.some((o) => o.value === ttl) && <option value={ttl}>当前：{ttl}</option>}
            </select>
          </label>
          <div className="cell-sub cfg-note">
            修改后立即生效：新登录按新时长计时，现有会话在下次请求时按新时长续期；
            缩短时长会同步收紧已有会话。过期后前端自动退出到登录页。
          </div>
        </div>
        <div className="cfg-actions">
          <button type="button" className="primary" onClick={saveTTL} disabled={ttlBusy}>
            {ttlBusy ? '保存中…' : '保存会话时长'}
          </button>
        </div>
        {ttlMsg && <div className={`notice ${ttlMsg.ok ? 'ok' : 'err'}`}>{ttlMsg.text}</div>}
      </Section>

      <Section
        id="files" title="文件管理" sub="在线预览/编辑上限与列表分页（默认每页 100 条）"
        open={open.files} onToggle={toggle}
      >
        <div className="cfg-grid">
          <label>
            单文件上限（MiB）
            <input
              value={editMB}
              onChange={(e) => setEditMB(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric"
              placeholder="10"
            />
            <span className="cell-sub">范围 1–64 MiB；超过上限的文件只能下载后本地打开。</span>
          </label>
          <label>
            列表每页条数
            <input
              value={listPageSize}
              onChange={(e) => setListPageSize(e.target.value.replace(/[^0-9]/g, ''))}
              inputMode="numeric"
              placeholder="100"
            />
            <span className="cell-sub">范围 20–1000 条；大目录分页加载，避免一次把整份列表压给服务端和浏览器。</span>
          </label>
          <div className="cell-sub cfg-note">
            支持 .log / .txt / .php / .ini / .xml / .json / .yaml 等文本格式。
            打开时会自动检测编码（UTF-8 / GB18030 / Big5 / UTF-16 …），也可以手动指定编码；
            保存按所选编码写回。二进制文件会被拒绝，不会在浏览器里打开。
          </div>
        </div>
        <div className="cfg-actions">
          <button type="button" className="primary" onClick={saveFileCfg} disabled={editBusy}>
            {editBusy ? '保存中…' : '保存文件管理配置'}
          </button>
        </div>
        {editMsg && <div className={`notice ${editMsg.ok ? 'ok' : 'err'}`}>{editMsg.text}</div>}
      </Section>

      <Section
        id="agent" title="Agent 监听配置" sub="Agent 长连接接入地址（默认本机外网 IP:9000）"
        open={open.agent} onToggle={toggle}
      >
        <div className="lat-msg warn">
          保存时先确保新监听绑定成功，再向在线 Agent 下发新地址：新版 Agent 落盘并重连，
          旧版 Agent 不会确认、可能失联。请先放通防火墙 / WAF 的对应端口，并确认新地址对 Agent 可达；
          绑定失败不会改动 Agent，监听切换失败会自动恢复旧地址。
        </div>
        <div className="cfg-grid">
          <label>
            监听 Host（留空 = 全部网卡）
            <input value={agentHost} onChange={(e) => setAgentHost(e.target.value)}
              placeholder="例如 1.2.3.4 或留空" />
            <span className="cell-sub">
              当前实际监听：<code>{cfg?.agent_listen?.addr || '—'}</code>
              {cfg?.public_ip ? `；本机外网 IP：${cfg.public_ip}` : ''}
            </span>
          </label>
          <label>
            监听端口
            <input value={agentPort} onChange={(e) => setAgentPort(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric" placeholder="9000" />
          </label>
          <label>
            当前登录密码（必填）
            <input type="password" value={agentPw} onChange={(e) => setAgentPw(e.target.value)}
              autoComplete="current-password" placeholder="验证后才允许修改监听" />
          </label>
          <div className="cfg-inline">
            <button type="button" className="btn ghost btn-sm" onClick={() => setAgentHost('')}>
              监听全部网卡
            </button>
            <button type="button" className="btn ghost btn-sm" onClick={() => setAgentHost(cfg?.public_ip || '')}
              disabled={!cfg?.public_ip}>
              使用本机外网 IP
            </button>
          </div>
        </div>
        <label className="cfg-check">
          <input type="checkbox" checked={agentConfirm} onChange={(e) => setAgentConfirm(e.target.checked)} />
          我已知晓：变更会使部分 Agent 短暂离线和重连；防火墙 / WAF 已放通新端口。
        </label>
        <div className="cfg-actions">
          <button type="button" className="primary" onClick={saveAgentListen} disabled={agentBusy}>
            {agentBusy ? '下发并切换中…' : '保存并切换 Agent 监听'}
          </button>
        </div>
        {agentMsg && <div className={`notice ${agentMsg.ok ? 'ok' : 'err'}`}>{agentMsg.text}</div>}
      </Section>

      <Section
        id="http" title="后台配置" sub="Web 控制台监听地址（默认本机外网 IP:8080）"
        open={open.http} onToggle={toggle}
      >
        <div className="lat-msg warn">
          保存后当前页面会断开，必须用新地址重新访问。同一端口换 Host 时会在响应返回后切换，
          失败会自动恢复旧监听；换端口时新监听先就绪再关闭旧入口。请先放通防火墙 / WAF。
        </div>
        <div className="cfg-grid">
          <label>
            监听 Host（留空 = 全部网卡）
            <input value={httpHost} onChange={(e) => setHttpHost(e.target.value)}
              placeholder="例如 1.2.3.4 或留空" />
            <span className="cell-sub">
              当前实际监听：<code>{cfg?.http_listen?.addr || '—'}</code>
            </span>
          </label>
          <label>
            监听端口
            <input value={httpPort} onChange={(e) => setHttpPort(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric" placeholder="8080" />
          </label>
          <label>
            当前登录密码（必填）
            <input type="password" value={httpPw} onChange={(e) => setHttpPw(e.target.value)}
              autoComplete="current-password" placeholder="验证后才允许修改监听" />
          </label>
          <div className="cfg-inline">
            <button type="button" className="btn ghost btn-sm" onClick={() => setHttpHost('')}>
              监听全部网卡
            </button>
            <button type="button" className="btn ghost btn-sm" onClick={() => setHttpHost(cfg?.public_ip || '')}
              disabled={!cfg?.public_ip}>
              使用本机外网 IP
            </button>
          </div>
        </div>
        <label className="cfg-check">
          <input type="checkbox" checked={httpConfirm} onChange={(e) => setHttpConfirm(e.target.checked)} />
          我已知晓：保存后当前页面会断开，需要用新地址访问；防火墙 / WAF 已放通新端口。
        </label>
        {httpNewUrl && <div className="cell-sub">新地址预计为：<code>{httpNewUrl}</code></div>}
        <div className="cfg-actions">
          <button type="button" className="primary" onClick={saveHTTPListen} disabled={httpBusy}>
            {httpBusy ? '切换中…' : '保存并切换后台监听'}
          </button>
        </div>
        {httpMsg && <div className={`notice ${httpMsg.ok ? 'ok' : 'err'}`}>{httpMsg.text}</div>}
      </Section>
    </div>
  )
}
