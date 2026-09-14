import React from 'react'

// 离线机器列表：记录历史注册过的主机，展示最后上线时间与离线时长
export default function Offline({ agents }) {
  const offline = (agents || []).filter((a) => !a.online)

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>离线机器</h2>
          <div className="sub">历史注册主机 · 共 {offline.length} 台离线</div>
        </div>
      </div>

      <div className="card">
        <div className="table-scroll">
        <table className="table-stack">
          <thead>
            <tr>
              <th>主机</th>
              <th>系统 / 架构</th>
              <th>最后上线时间</th>
              <th>离线时长</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            {(offline || []).map((a) => (
              <tr key={a.agent_id}>
                <td data-label="主机">
                  <b>{a.remark || a.hostname}</b>
                  <div style={{ color: '#8b98a5', fontSize: 12 }}>{a.hostname !== a.agent_id ? a.hostname + ' · ' : ''}{a.agent_id}</div>
                </td>
                <td data-label="系统/架构">{a.os_version || a.os_type} / {a.arch}</td>
                <td data-label="最后上线">{fmtTime(a.last_seen)}</td>
                <td data-label="离线时长">{offlineDuration(a)}</td>
                <td data-label="状态"><span className="badge offline">离线</span></td>
              </tr>
            ))}
            {offline.length === 0 && (
              <tr><td colSpan="5" className="table-empty">当前没有离线主机</td></tr>
            )}
          </tbody>
        </table>
        </div>
      </div>
    </div>
  )
}

function fmtTime(ts) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

function offlineDuration(a) {
  const end = a.last_offline_at || Date.now() / 1000
  const diff = Math.max(0, end - (a.last_seen || 0))
  const s = Math.floor(diff)
  if (s < 60) return `${s}秒`
  if (s < 3600) return `${Math.floor(s / 60)}分`
  if (s < 86400) return `${Math.floor(s / 3600)}时`
  return `${Math.floor(s / 86400)}天`
}
