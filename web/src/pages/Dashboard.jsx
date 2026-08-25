import React, { useState } from 'react'

// 主机探针信息面板：展示各主机 CPU/内存/磁盘/流量等实时状态
export default function Dashboard({ agents }) {
  const [since, setSince] = useState('24h')

  const online = agents.filter((a) => a.online).length
  const offline = agents.filter((a) => !a.online).length

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
              </tr>
            ))}
            {agents.length === 0 && (
              <tr><td colSpan="9" style={{ textAlign: 'center', color: '#8b98a5' }}>暂无主机接入</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
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
