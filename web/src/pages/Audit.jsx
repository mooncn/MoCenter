import React, { useEffect, useState, useCallback, useRef } from 'react'
import { Api } from '../api/api.js'
import Pagination from '../components/Pagination.jsx'

// 操作审计：命令执行 / 运维操作 / 登录认证记录的检索与导出。
// 数据量会持续增长，因此采用「服务端分页 + 条件筛选」，
// 前端只加载当前页，避免一次性拉取全量日志。
export default function Audit() {
  const [items, setItems] = useState([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [category, setCategory] = useState('')
  const [keyword, setKeyword] = useState('')
  const [operator, setOperator] = useState('')
  const [range, setRange] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [expanded, setExpanded] = useState(null)
  const timer = useRef(0)

  // range -> { since, until }（秒级时间戳）
  const rangeParams = () => {
    const now = Math.floor(Date.now() / 1000)
    switch (range) {
      case '1h': return { since: now - 3600 }
      case '24h': return { since: now - 86400 }
      case '7d': return { since: now - 7 * 86400 }
      case '30d': return { since: now - 30 * 86400 }
      default: return {}
    }
  }

  const load = useCallback(async (p = page, size = pageSize) => {
    setLoading(true)
    try {
      const { since, until } = rangeParams()
      const res = await Api.audits({
        offset: (p - 1) * size,
        limit: size,
        cmd: keyword.trim(),
        operator: operator.trim(),
        category,
        since,
        until,
      })
      setItems(res.items)
      setTotal(res.total)
      setError('')
    } catch (err) {
      setError(err?.response?.data?.msg || err?.message || '获取审计日志失败')
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, keyword, operator, category, range])

  // 首次与筛选条件变化时回到第 1 页并重新加载（关键字输入做 300ms 防抖）
  useEffect(() => {
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => {
      setPage(1)
      load(1)
    }, 300)
    return () => window.clearTimeout(timer.current)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keyword, operator, category, range, pageSize])

  const goto = (p) => {
    setPage(p)
    load(p, pageSize)
  }

  const exportCsv = async () => {
    try {
      const { since, until } = rangeParams()
      // 导出时按当前筛选条件一次取较多行（上限 500/页），避免导出受限
      const blob = await Api.auditsCsv({
        offset: 0, limit: 500, cmd: keyword.trim(), operator: operator.trim(), category, since, until,
      })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `audits-${new Date().toISOString().slice(0, 10)}.csv`
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) {
      setError('导出失败：' + (e?.response?.data?.msg || e?.message))
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>操作审计</h2>
          <div className="sub">命令执行 / 运维操作 / 登录认证留痕 · 服务端分页检索，满足安全合规要求</div>
        </div>
        <button className="secondary" onClick={exportCsv}>导出 CSV</button>
      </div>

      <div className="card">
        {error && <div className="notice err" style={{ marginBottom: 12 }}>{error}</div>}

        <div className="filter-bar">
          <div className="form-row">
            <label>类别</label>
            <select value={category} onChange={(e) => setCategory(e.target.value)}>
              <option value="">全部类别</option>
              <option value="command">命令执行</option>
              <option value="op">运维操作</option>
              <option value="auth">登录认证</option>
            </select>
          </div>
          <div className="form-row">
            <label>命令 / 主机关键字</label>
            <input value={keyword} onChange={(e) => setKeyword(e.target.value)} placeholder="如 uptime / host-01" />
          </div>
          <div className="form-row">
            <label>操作人</label>
            <input value={operator} onChange={(e) => setOperator(e.target.value)} placeholder="如 admin" />
          </div>
          <div className="form-row">
            <label>时间范围</label>
            <select value={range} onChange={(e) => setRange(e.target.value)}>
              <option value="">全部时间</option>
              <option value="1h">最近 1 小时</option>
              <option value="24h">最近 24 小时</option>
              <option value="7d">最近 7 天</option>
              <option value="30d">最近 30 天</option>
            </select>
          </div>
          <div className="form-row filter-actions">
            <button className="secondary" onClick={() => { setKeyword(''); setOperator(''); setCategory(''); setRange('') }}>重置</button>
            <button onClick={() => load(page, pageSize)} disabled={loading}>刷新</button>
          </div>
        </div>

        <div className="table-scroll">
          <table className="table-stack">
            <thead>
              <tr>
                <th>时间</th>
                <th>类别</th>
                <th>操作人</th>
                <th>目标主机</th>
                <th>操作内容</th>
                <th>结果</th>
                <th>退出码</th>
                <th>耗时</th>
              </tr>
            </thead>
            <tbody>
              {items.map((l, i) => (
                <tr key={l.id || i} onClick={() => setExpanded(expanded === (l.id || i) ? null : (l.id || i))}>
                  <td data-label="时间">{fmtTime(l.time)}</td>
                  <td data-label="类别"><span className={`badge cat-${l.category || 'command'}`}>{catName(l.category)}</span></td>
                  <td data-label="操作人">{l.operator || '-'}{l.operator_ip ? <div className="cell-sub">{l.operator_ip}</div> : null}</td>
                  <td data-label="目标主机">{l.agent_id || '-'}</td>
                  <td data-label="操作内容" className="cell-clamp" title={l.command}>{l.command}</td>
                  <td data-label="结果">
                    {l.blocked
                      ? <span className="badge offline">已拦截</span>
                      : l.exit_code === -1
                        ? <span className="badge offline">失败</span>
                        : l.result
                          ? <span className="badge online">成功</span>
                          : <span className="badge idle">执行中</span>}
                  </td>
                  <td data-label="退出码">{l.exit_code}</td>
                  <td data-label="耗时">{l.duration}ms</td>
                </tr>
              ))}
              {!loading && items.length === 0 && (
                <tr><td colSpan="8" className="table-empty">暂无审计记录</td></tr>
              )}
              {loading && (
                <tr><td colSpan="8" className="table-empty">加载中...</td></tr>
              )}
            </tbody>
          </table>
        </div>

        {expanded !== null && (() => {
          const l = items.find((x, i) => (x.id || i) === expanded)
          if (!l) return null
          return (
            <div className="detail-panel">
              <div><b>完整操作内容：</b><pre className="pre-wrap">{l.command || '-'}</pre></div>
              {l.result && <div style={{ marginTop: 8 }}><b>执行结果：</b><pre className="pre-wrap">{l.result}</pre></div>}
              {l.blocked && <div style={{ marginTop: 8, color: 'var(--red)' }}>拦截原因：{l.block_reason || '-'}</div>}
            </div>
          )
        })()}

        <Pagination
          total={total}
          page={page}
          pageSize={pageSize}
          onPage={goto}
          onPageSize={(n) => { setPageSize(n); setPage(1) }}
          loading={loading}
        />
      </div>
    </div>
  )
}

function catName(c) {
  switch (c) {
    case 'op': return '运维操作'
    case 'auth': return '登录认证'
    default: return '命令执行'
  }
}

function fmtTime(ts) {
  if (!ts) return '-'
  const d = new Date(ts * 1000)
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}
