import React, { useState, useEffect } from 'react'
import { Api } from '../api/api.js'

// 主机探针信息面板：展示各主机 CPU/内存/磁盘/流量等实时状态与历史趋势
export default function Dashboard({ agents }) {
  const [since, setSince] = useState('24h')
  const [selectedAgent, setSelectedAgent] = useState('')
  const [history, setHistory] = useState(null)
  // 服务端存活状态：null=检测中 true=在线 false=离线
  const [serverAlive, setServerAlive] = useState(null)

  const online = (agents || []).filter((a) => a.online).length
  const offline = (agents || []).filter((a) => !a.online).length

  const sinceMap = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800 }

  // 每 5 秒探测服务端存活状态（/api/health 无需鉴权）
  useEffect(() => {
    let alive = true
    const check = async () => {
      try { await Api.health(); alive && setServerAlive(true) }
      catch { alive && setServerAlive(false) }
    }
    check()
    const t = setInterval(check, 5000)
    return () => { alive = false; clearInterval(t) }
  }, [])

  // 加载选中主机的历史探针趋势
  useEffect(() => {
    if (!selectedAgent) {
      setHistory(null)
      return
    }
    const sinceTs = new Date(Date.now() - sinceMap[since] * 1000).toISOString()
    Api.history(selectedAgent, sinceTs)
      .then((data) => setHistory(data))
      .catch(() => setHistory([]))
  }, [selectedAgent, since])

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>仪表盘</h2>
          <div className="sub">主机探针信息总览 · 在线 {online} / 离线 {offline} / 共 {agents.length}</div>
        </div>
        <span className={`badge ${serverAlive === false ? 'offline' : serverAlive ? 'online' : ''}`} style={{ marginRight: 12 }}>
          <span className={`dot ${serverAlive === false ? 'offline' : serverAlive ? 'online' : ''}`} />{' '}
          {serverAlive === null ? '服务端检测中…' : serverAlive ? '服务端在线' : '服务端离线'}
        </span>
        <select value={since} onChange={(e) => setSince(e.target.value)} style={{ width: 150 }}>
          <option value="1h">最近 1 小时</option>
          <option value="6h">最近 6 小时</option>
          <option value="24h">最近 24 小时</option>
          <option value="7d">最近 7 天</option>
        </select>
      </div>

      {/* 汇总指标 */}
      <div className="metrics" style={{ marginBottom: 20 }}>
        <div className="metric">
          <div className="value">{online}</div>
          <div className="label">在线主机</div>
        </div>
        <div className="metric">
          <div className="value">{offline}</div>
          <div className="label">离线主机</div>
        </div>
        <div className="metric">
          <div className="value">{agents.length}</div>
          <div className="label">注册主机</div>
        </div>
        <div className="metric">
          <div className="value">{(agents || []).length > 0 ? Math.round((agents || []).reduce((s, a) => s + a.cpu_usage, 0) / agents.length) : 0}%</div>
          <div className="label">平均 CPU</div>
        </div>
        <div className="metric">
          <div className="value">{(agents || []).length > 0 ? Math.round((agents || []).reduce((s, a) => s + a.mem_usage, 0) / agents.length) : 0}%</div>
          <div className="label">平均内存</div>
        </div>
      </div>

      {/* 主机列表 */}
      <div className="card">
        <h3>主机列表</h3>
        <table>
          <thead>
            <tr>
              <th>主机</th>
              <th>状态</th>
              <th>公网 IP / 地理位置</th>
              <th>系统</th>
              <th>架构</th>
              <th>CPU</th>
              <th>内存</th>
              <th>磁盘</th>
              <th>网络 ↓ / ↑</th>
              <th>最后在线</th>
              <th>趋势</th>
            </tr>
          </thead>
          <tbody>
            {(agents || []).map((a) => (
              <tr key={a.agent_id}>
                <td>
                  <b>{a.remark || a.hostname}</b>
                  <div style={{ color: '#8b98a5', fontSize: 12 }}>
                    {a.hostname}
                    {a.remark ? ' · ' + a.agent_id : ''}
                  </div>
                  <RemarkEditor agent={a} onSaved={() => window.dispatchEvent(new Event('mc-remark-saved'))} />
                </td>
                <td>
                  <span className={`badge ${a.online ? 'online' : 'offline'}`}>
                    {a.online ? '在线' : '离线'}
                  </span>
                </td>
                <td>
                  <PublicIPCell agent={a} />
                </td>
                <td>{a.os_type}</td>
                <td>{a.arch}</td>
                <td><CpuBar v={a.cpu_usage} /></td>
                <td><MemBar v={a.mem_usage} /></td>
                <td><Bar v={a.disk_usage} /></td>
                <td>{fmtBytes(a.net_in_bytes)} / {fmtBytes(a.net_out_bytes)}</td>
                <td>{a.online ? fmtTime(a.last_seen) : `离线 ${fmtOffline(a)}`}</td>
                <td>
                  <button className="secondary" onClick={() => setSelectedAgent(selectedAgent === a.agent_id ? '' : a.agent_id)}>
                    {selectedAgent === a.agent_id ? '收起' : '查看'}
                  </button>
                </td>
              </tr>
            ))}
            {agents.length === 0 && (
              <tr><td colSpan="11" style={{ textAlign: 'center', color: '#8b98a5' }}>暂无主机接入</td></tr>
            )}
          </tbody>
        </table>
      </div>

      {/* 历史趋势图 */}
      {selectedAgent && (
        <div className="card" style={{ marginTop: 20 }}>
          <h3>探针历史趋势 · {agents.find((a) => a.agent_id === selectedAgent)?.hostname}</h3>
          <TrendChart history={history || []} />
        </div>
      )}
    </div>
  )
}

// 轻量 SVG 趋势折线图（无外部依赖）
function TrendChart({ history }) {
  const W = 900, H = 240, PAD = 30
  if (!history || history.length === 0) {
    return <div style={{ color: '#8b98a5', textAlign: 'center', padding: 30 }}>该时间段暂无历史采样数据</div>
  }

  const series = history.slice(-200) // 最多展示 200 个点
  const draw = (key, color) => {
    const vals = series.map((p) => p[key] || 0)
    const max = Math.max(100, ...vals)
    const pts = vals.map((v, i) => {
      const x = PAD + (i / (series.length - 1)) * (W - PAD * 2)
      const y = H - PAD - (v / max) * (H - PAD * 2)
      return `${x.toFixed(1)},${y.toFixed(1)}`
    }).join(' ')
    const last = vals[vals.length - 1]
    return (
      <g key={key}>
        <polyline points={pts} fill="none" stroke={color} strokeWidth="2" />
        <text x={W - PAD - 8} y={H - PAD - (last / max) * (H - PAD * 2) + 4} fill={color} fontSize="11" textAnchor="end">{Math.round(last)}%</text>
      </g>
    )
  }

  return (
    <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 'auto' }}>
      {/* 网格线 */}
      {[0, 25, 50, 75, 100].map((g) => {
        const y = H - PAD - (g / 100) * (H - PAD * 2)
        return <g key={g}>
          <line x1={PAD} y1={y} x2={W - PAD} y2={y} stroke="#2a3340" strokeDasharray="4 4" />
          <text x={PAD - 6} y={y + 4} fill="#8b98a5" fontSize="10" textAnchor="end">{g}%</text>
        </g>
      })}
      {draw('cpu_usage', '#4da3ff')}
      {draw('mem_usage', '#52c41a')}
      {draw('disk_usage', '#fa8c16')}
      <g>
        <rect x={PAD} y={4} width={10} height={10} fill="#4da3ff" />
        <text x={PAD + 14} y={13} fill="#8b98a5" fontSize="11">CPU</text>
        <rect x={PAD + 60} y={4} width={10} height={10} fill="#52c41a" />
        <text x={PAD + 74} y={13} fill="#8b98a5" fontSize="11">内存</text>
        <rect x={PAD + 120} y={4} width={10} height={10} fill="#fa8c16" />
        <text x={PAD + 134} y={13} fill="#8b98a5" fontSize="11">磁盘</text>
      </g>
    </svg>
  )
}

function Bar({ v }) {
  const val = Math.max(0, Math.min(100, v || 0))
  const cls = val > 85 ? 'danger' : val > 60 ? 'warn' : ''
  return (
    <div>
      <div style={{ fontSize: 12 }}>{val.toFixed(1)}%</div>
      <div className={`bar ${cls}`}><div style={{ width: `${val}%` }} /></div>
    </div>
  )
}
const CpuBar = Bar
const MemBar = Bar

function fmtBytes(b) {
  if (!b) return '0'
  if (b > 1024 * 1024 * 1024) return (b / 1024 / 1024 / 1024).toFixed(1) + 'G'
  if (b > 1024 * 1024) return (b / 1024 / 1024).toFixed(1) + 'M'
  if (b > 1024) return (b / 1024).toFixed(1) + 'K'
  return b + 'B'
}

function fmtTime(ts) {
  if (!ts) return '-'
  const d = new Date(ts * 1000)
  return d.toLocaleString()
}

function fmtOffline(a) {
  const diff = (Date.now() / 1000) - (a.last_offline_at || a.last_seen || 0)
  const s = Math.max(0, Math.floor(diff))
  if (s < 60) return `${s}秒`
  if (s < 3600) return `${Math.floor(s / 60)}分`
  if (s < 86400) return `${Math.floor(s / 3600)}时`
  return `${Math.floor(s / 86400)}天`
}

// 根据 ISO 国家代码返回国旗 emoji（零依赖，基于区域指示符字符）。
function countryFlag(code) {
  if (!code || code.length !== 2) return ''
  const base = 0x1F1E6
  const a = 'A'.charCodeAt(0)
  const upper = code.toUpperCase()
  return String.fromCodePoint(base + (upper.charCodeAt(0) - a), base + (upper.charCodeAt(1) - a))
}

// 公网 IP 与地理位置展示单元格。
function PublicIPCell({ agent }) {
  const ips = [agent.public_ip, agent.public_ipv6].filter(Boolean)
  const geo = agent.geo
  return (
    <div>
      {ips.length === 0 ? (
        <span style={{ color: '#8b98a5', fontSize: 12 }}>未获取到公网 IP</span>
      ) : (
        ips.map((ip, i) => (
          <div key={i} style={{ fontSize: 12 }} title={ip.includes(':') ? 'IPv6' : 'IPv4'}>
            <span style={{ color: '#8b98a5' }}>{ip.includes(':') ? 'v6' : 'v4'}</span>{' '}
            {ip}
          </div>
        ))
      )}
      {geo && geo.country_code && (
        <div style={{ fontSize: 12, color: '#4da3ff', marginTop: 2 }}>
          <span style={{ marginRight: 4 }}>{countryFlag(geo.country_code)}</span>
          {[geo.country, geo.region, geo.city].filter(Boolean).join(' · ')}
        </div>
      )}
    </div>
  )
}

// 备注编辑器：点击主机的备注图标可编辑。
function RemarkEditor({ agent, onSaved }) {
  const [editing, setEditing] = React.useState(false)
  const [value, setValue] = React.useState(agent.remark || '')
  const [saving, setSaving] = React.useState(false)

  const save = async () => {
    setSaving(true)
    try {
      await Api.setRemark(agent.agent_id, value.trim())
      onSaved && onSaved()
      setEditing(false)
    } catch (e) {
      alert('保存备注失败：' + (e.response?.data?.msg || e.message))
    } finally {
      setSaving(false)
    }
  }

  if (!editing) {
    return (
      <div style={{ marginTop: 2 }}>
        <button
          className="secondary"
          style={{ fontSize: 11, padding: '1px 6px' }}
          onClick={() => { setValue(agent.remark || ''); setEditing(true) }}
          title="编辑备注"
        >✎ 备注</button>
      </div>
    )
  }
  return (
    <div style={{ marginTop: 4, display: 'flex', gap: 4, alignItems: 'center' }}>
      <input
        value={value}
        onChange={(e) => setValue(e.target.value)}
        placeholder="输入备注…"
        autoFocus
        style={{ fontSize: 12, padding: '2px 6px', width: 120 }}
        onKeyDown={(e) => { if (e.key === 'Enter') save(); if (e.key === 'Escape') setEditing(false) }}
      />
      <button className="primary" style={{ fontSize: 11, padding: '2px 8px' }} onClick={save} disabled={saving}>
        {saving ? '保存中…' : '保存'}
      </button>
      <button className="secondary" style={{ fontSize: 11, padding: '2px 8px' }} onClick={() => setEditing(false)}>取消</button>
    </div>
  )
}
