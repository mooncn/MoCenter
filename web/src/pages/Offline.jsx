import React from 'react'
import { Api } from '../api/api.js'

// 离线机器列表：记录历史注册过的主机，展示最后上线时间与离线时长
export default function Offline({ agents }) {
  // 按最后上线时间倒序：服务端返回顺序不保证稳定，直接渲染会让列表在每次
  // 主机上报（ws 推送）后跳动，运维很难点中目标行。
  const offline = (agents || [])
    .filter((a) => !a.online)
    .slice()
    .sort((x, y) => (y.last_seen || 0) - (x.last_seen || 0))

  const removeAgent = async (a) => {
    const name = a.remark || a.hostname || a.agent_id
    const ok = window.confirm(
      `删除主机「${name}」？\n\n` +
      '将清除本服务端保存的主机记录、备注、探针/延迟历史与端口映射规则。\n' +
      '不会卸载目标机上的 Agent，也不清除证书绑定与未使用的注册令牌。\n\n' +
      '若该机 Agent 仍在运行，它会重新注册并再次出现。'
    )
    if (!ok) return
    try {
      const d = await Api.deleteAgent(a.agent_id)
      if (!d?.ok) { alert(d?.msg || '删除失败'); return }
      window.dispatchEvent(new Event('mc-agents-refresh'))
    } catch (e) {
      alert('删除失败：' + (e.response?.data?.msg || e.message))
    }
  }

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
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {(offline || []).map((a) => (
              <tr key={a.uuid || a.agent_id}>
                <td data-label="主机">
                  <b>{a.remark || a.hostname}</b>
                  <div style={{ color: '#8b98a5', fontSize: 12 }}>{a.hostname !== a.agent_id ? a.hostname + ' · ' : ''}{a.agent_id}</div>
                  {a.uuid ? (
                    <div
                      style={{ color: '#6b7280', fontSize: 10, fontFamily: 'Menlo, Consolas, monospace', maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                      title={`设备唯一 UUID：${a.uuid}`}
                    >
                      UUID: {a.uuid}
                    </div>
                  ) : null}
                </td>
                <td data-label="系统/架构">{a.os_version || a.os_type} / {a.arch}</td>
                <td data-label="最后上线">{fmtTime(a.last_seen)}</td>
                <td data-label="离线时长">{offlineDuration(a)}</td>
                <td data-label="状态"><span className="badge offline">离线</span></td>
                <td data-label="操作">
                  <button
                    className="secondary danger btn-sm"
                    onClick={() => removeAgent(a)}
                    title="删除这台离线主机的登记与历史数据"
                  >删除</button>
                </td>
              </tr>
            ))}
            {offline.length === 0 && (
              <tr><td colSpan="6" className="table-empty">当前没有离线主机</td></tr>
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

// 离线时长：从"最后上线"到"离线时刻"。
//
// 注意 last_offline_at 可能缺失或早于 last_seen（旧库数据、标离线时被覆盖），
// 此时回退到 last_seen，不能直接用 now 兜底 —— 否则"最后上线=昨天"会显示成
// "离线 1 天"而不是"离线 0 秒"，与列表里的时间自相矛盾。
function offlineDuration(a) {
  const start = a.last_seen || 0
  if (!start) return '-'
  let end = a.last_offline_at || start
  if (end < start) end = start
  return fmtDuration(Math.max(0, end - start))
}

// 秒 -> 可读时长
function fmtDuration(s) {
  if (s < 60) return `${Math.floor(s)}秒`
  if (s < 3600) return `${Math.floor(s / 60)}分`
  if (s < 86400) return `${Math.floor(s / 3600)}时`
  return `${Math.floor(s / 86400)}天`
}
