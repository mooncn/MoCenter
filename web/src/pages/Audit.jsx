import React, { useEffect, useState } from 'react'
import { Api } from '../api/api.js'

// 操作审计日志：查看命令执行等关键操作记录
export default function Audit() {
  const [logs, setLogs] = useState([])
  const [keyword, setKeyword] = useState('')
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    Api.audits()
      .then((data) => setLogs(data))
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [])

  const filtered = logs.filter((l) => {
    if (!keyword) return true
    return (l.command || '').includes(keyword) || (l.agent_id || '').includes(keyword)
  })

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>操作审计</h2>
          <div className="sub">记录所有命令执行等关键操作，满足安全合规要求</div>
        </div>
      </div>

      <div className="card">
        <div className="form-row" style={{ maxWidth: 400 }}>
          <label>按命令 / 主机关键字检索</label>
          <input value={keyword} onChange={(e) => setKeyword(e.target.value)} placeholder="输入关键字过滤..." />
        </div>
        <table>
          <thead>
            <tr>
              <th>时间</th>
              <th>操作人</th>
              <th>目标主机</th>
              <th>命令</th>
              <th>结果</th>
              <th>退出码</th>
              <th>耗时</th>
            </tr>
          </thead>
          <tbody>
            {filtered.slice().reverse().map((l, i) => (
              <tr key={i}>
                <td>{fmtTime(l.time)}</td>
                <td>{l.operator || '-'}</td>
                <td>{l.agent_id}</td>
                <td style={{ maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {l.command}
                </td>
                <td>
                  {l.blocked ? (
                    <span className="badge offline">已拦截</span>
                  ) : l.result ? (
                    <span className="badge online">成功</span>
                  ) : (
                    <span className="badge offline">失败</span>
                  )}
                </td>
                <td>{l.exit_code}</td>
                <td>{l.duration}ms</td>
              </tr>
            ))}
            {!loading && filtered.length === 0 && (
              <tr><td colSpan="7" style={{ textAlign: 'center', color: '#8b98a5' }}>暂无审计记录</td></tr>
            )}
            {loading && (
              <tr><td colSpan="7" style={{ textAlign: 'center', color: '#8b98a5' }}>加载中...</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function fmtTime(ts) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}
