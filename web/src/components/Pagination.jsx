import React, { useMemo } from 'react'

// 通用分页组件：配合服务端分页使用（数据量持续增长时不一次性加载全量）。
// props:
//   total     总记录数
//   page      当前页（从 1 开始）
//   pageSize  每页条数
//   onPage    切换页码回调
//   onPageSize 切换每页条数回调（可选）
//   loading   加载中标记
export default function Pagination({ total = 0, page = 1, pageSize = 20, onPage, onPageSize, loading = false }) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const cur = Math.min(Math.max(1, page), totalPages)

  // 页码窗口：始终显示首尾页 + 当前页附近，其余用省略号折叠
  const pages = useMemo(() => {
    const list = []
    const push = (v) => { if (!list.includes(v)) list.push(v) }
    const window = 1
    push(1)
    for (let i = cur - window; i <= cur + window; i++) {
      if (i > 1 && i < totalPages) push(i)
    }
    push(totalPages)
    list.sort((a, b) => a - b)
    const out = []
    let prev = 0
    for (const p of list) {
      if (prev && p - prev > 1) out.push('...')
      out.push(p)
      prev = p
    }
    return out
  }, [cur, totalPages])

  const from = total === 0 ? 0 : (cur - 1) * pageSize + 1
  const to = Math.min(cur * pageSize, total)

  return (
    <div className="pagination">
      <div className="pagination-info">
        共 <b>{total}</b> 条{total > 0 ? ` · 第 ${from}-${to} 条` : ''} · 第 {cur}/{totalPages} 页
      </div>
      <div className="pagination-controls">
        <button className="secondary btn-sm" onClick={() => onPage?.(1)} disabled={cur <= 1 || loading}>首页</button>
        <button className="secondary btn-sm" onClick={() => onPage?.(cur - 1)} disabled={cur <= 1 || loading}>上一页</button>
        {pages.map((p, i) =>
          p === '...' ? (
            <span key={`gap-${i}`} className="pagination-gap">…</span>
          ) : (
            <button
              key={p}
              className={p === cur ? 'btn-sm' : 'secondary btn-sm'}
              onClick={() => onPage?.(p)}
              disabled={loading}
            >{p}</button>
          )
        )}
        <button className="secondary btn-sm" onClick={() => onPage?.(cur + 1)} disabled={cur >= totalPages || loading}>下一页</button>
        <button className="secondary btn-sm" onClick={() => onPage?.(totalPages)} disabled={cur >= totalPages || loading}>末页</button>
        {onPageSize && (
          <select
            className="pagination-size"
            value={pageSize}
            onChange={(e) => onPageSize(Number(e.target.value))}
            disabled={loading}
            title="每页条数"
          >
            {[10, 20, 50, 100].map((n) => <option key={n} value={n}>{n} 条/页</option>)}
          </select>
        )}
      </div>
    </div>
  )
}
