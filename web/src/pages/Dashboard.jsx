import React, { useState, useEffect } from 'react'
import { Api } from '../api/api.js'

// 主机探针信息面板：展示各主机 CPU/内存/磁盘/流量等实时状态与历史趋势
export default function Dashboard({ agents }) {
  const [since, setSince] = useState('24h')
  const [selectedAgent, setSelectedAgent] = useState('')
  const [history, setHistory] = useState(null)

  const online = agents.filter((a) => a.online).length
  const offline = agents.filter((a) => !a.online).length

  const sinceMap = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800 }

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
          <div className="value">{agents.length ? Math.round(agents.reduce((s, a) => s + a.cpu_usage, 0) / agents.length) : 0}%</div>
          <div className="label">平均 CPU</div>
        </div>
        <div className="metric">
          <div className="value">{agents.length ? Math.round(agents.reduce((s, a) => s + a.mem_usage, 0) / agents.length) : 0}%</div>
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
            {agents.map((a) => (
              <tr key={a.agent_id}>
                <td>
                  <b>{a.hostname}</b>
                  <div style={{ color: '#8b98a5', fontSize: 12 }}>{a.agent_id}</div>
                </td>
                <td>
                  <span className={`badge ${a.online ? 'online' : 'offline'}`}>
                    {a.online ? '在线' : '离线'}
                  </span>
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
              <tr><td colSpan="10" style={{ textAlign: 'center', color: '#8b98a5' }}>暂无主机接入</td></tr>
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
