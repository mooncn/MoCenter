import React, { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { PublicApi, PublicSocket } from '../api/api.js'

// 公开状态页（**免登录**）：展示非敏感运行指标 + 节点详情（历史趋势）。
//
// 数据来源与安全边界：
//   - 只调用 /api/public/*，响应由服务端 publicview 脱敏后生成，
//     未开启的字段在 JSON 里"根本不存在"，因此这里无需（也不能）自行判断脱敏。
//   - 节点标识是服务端派生的 node_id（n-xxxxxxxx），前端拿不到 agent_id。
//   - 总开关关闭时接口返回 404，本页显示"未开放"，不做任何兜底猜测。
export default function PublicStatus() {
  // snapshot 是一次完整的展示快照：config + fields + nodes + summary。
  // 关键：节点列表与汇总必须**同源同帧**更新 —— 否则 WS 推送先到、汇总后到，
  // 会出现"卡片已有数据、汇总还显示 0"的自相矛盾画面。
  const [snapshot, setSnapshot] = useState(null)
  const [loading, setLoading] = useState(true)
  const [disabled, setDisabled] = useState(false)
  const [view, setView] = useState('card') // card | list
  const socketRef = useRef(null)

  useEffect(() => {
    let alive = true
    PublicApi.state()
      .then((d) => {
        if (!alive) return
        if (!d || !d.enabled) {
          setDisabled(true)
          return
        }
        setSnapshot({
          config: d.config || {},
          fields: d.fields || [],
          nodes: Array.isArray(d.nodes) ? d.nodes : [],
          summary: d.summary || {},
        })
      })
      .catch((err) => {
        // 404 = 未启用；其它错误按"暂时不可用"处理，避免误导为已关闭
        if (alive) setDisabled(err.response?.status === 404)
      })
      .finally(() => alive && setLoading(false))

    // 实时推送：服务端在主机上线/探针更新/离线时推脱敏后的节点列表与汇总
    const sock = new PublicSocket({
      onData: (d) => {
        if (!d || !Array.isArray(d.nodes)) return
        setSnapshot((prev) => ({
          ...prev,
          config: d.config || prev?.config || {},
          fields: prev?.fields || [],
          nodes: d.nodes,
          summary: d.summary || {},
        }))
        setDisabled(false)
      },
    })
    sock.connect()
    socketRef.current = sock

    // 页面隐藏时断开、可见时重连：后台标签页不必长期占用一个服务端连接
    const onVisibility = () => {
      if (document.hidden) sock.close()
      else { sock.alive = true; sock.connect() }
    }
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      alive = false
      document.removeEventListener('visibilitychange', onVisibility)
      sock.close()
    }
  }, [])

  if (loading) {
    return <div className="public-shell"><div className="public-empty">正在加载节点状态…</div></div>
  }
  if (disabled) {
    return (
      <div className="public-shell">
        <div className="public-empty">
          <div style={{ fontSize: 34, marginBottom: 10 }}>🔒</div>
          <div style={{ fontSize: 16, marginBottom: 6 }}>本站未开放公开状态页</div>
          <div style={{ color: 'var(--muted)', fontSize: 13 }}>
            公开状态页需要在后台「展示配置」中开启总开关后才会生效。
          </div>
          <div style={{ marginTop: 18 }}>
            <Link className="public-btn" to="/login">登录管理后台</Link>
          </div>
        </div>
      </div>
    )
  }

  const config = snapshot?.config || {}
  const nodes = snapshot?.nodes || []
  const summary = snapshot?.summary || {}
  return (
    <div className="public-shell">
      <header className="public-head">
        <div>
          <h1>🖥 {config.title || 'MoCenter 节点状态'}</h1>
          <div className="public-desc">{config.description}</div>
        </div>
        <div className="public-head-actions">
          {config.hosts_enabled !== false && nodes.length > 0 && (
            <div className="seg">
              <button className={view === 'card' ? 'on' : ''} onClick={() => setView('card')}>卡片</button>
              <button className={view === 'list' ? 'on' : ''} onClick={() => setView('list')}>列表</button>
            </div>
          )}
          <Link className="public-btn ghost" to="/login">登录后台</Link>
        </div>
      </header>

      <SummaryBar summary={summary} />

      {config.hosts_enabled === false ? (
        <div className="public-empty">站点仅公开汇总指标，主机明细未开放。</div>
      ) : nodes.length === 0 ? (
        <div className="public-empty">当前没有可展示的节点。</div>
      ) : view === 'card' ? (
        <NodeCards nodes={nodes} />
      ) : (
        <NodeTable nodes={nodes} />
      )}

      <footer className="public-foot">
        数据每 5 秒由 Agent 上报 · 页面实时刷新 · <Link to="/login">管理后台</Link>
      </footer>
    </div>
  )
}

// 站点级汇总（不含任何主机标识）
function SummaryBar({ summary }) {
  const items = [
    { label: '节点总数', value: summary.total ?? 0 },
    { label: '在线', value: summary.online ?? 0, tone: 'ok' },
    { label: '离线', value: summary.offline ?? 0, tone: (summary.offline ?? 0) > 0 ? 'bad' : '' },
    { label: '平均 CPU', value: `${summary.avg_cpu ?? 0}%` },
    { label: '平均内存', value: `${summary.avg_mem ?? 0}%` },
    { label: '平均磁盘', value: `${summary.avg_disk ?? 0}%` },
  ]
  return (
    <div className="public-metrics">
      {items.map((it) => (
        <div className={`public-metric ${it.tone || ''}`} key={it.label}>
          <div className="v">{it.value}</div>
          <div className="l">{it.label}</div>
        </div>
      ))}
    </div>
  )
}

// 卡片视图（移动端友好）
function NodeCards({ nodes }) {
  const nav = useNavigate()
  return (
    <div className="public-cards">
      {nodes.map((n) => (
        <button className="public-card" key={n.node_id} onClick={() => nav(`/public/node/${n.node_id}`)}>
          <div className="public-card-head">
            <span className="public-card-name">{n.remark || n.node_id}</span>
            {n.online !== undefined && (
              <span className={`badge ${n.online ? 'online' : 'offline'}`}>{n.online ? '在线' : '离线'}</span>
            )}
          </div>
          <div className="public-card-meta">
            {n.os_type && <span>{n.os_type}{n.arch ? ` · ${n.arch}` : ''}</span>}
            {n.geo && <span>{n.geo.country_code} {[n.geo.country, n.geo.region, n.geo.city].filter(Boolean).join(' ')}</span>}
          </div>
          <MiniBar label="CPU" value={n.cpu_usage} />
          <MiniBar label="内存" value={n.mem_usage} sub={bytesPair(n.mem_used_bytes, n.mem_total_bytes)} />
          <MiniBar label="磁盘" value={n.disk_usage} sub={bytesPair(n.disk_used_bytes, n.disk_total_bytes)} />
          <div className="public-card-foot">查看历史趋势 →</div>
        </button>
      ))}
    </div>
  )
}

// 列表视图（信息密度高）
function NodeTable({ nodes }) {
  const nav = useNavigate()
  return (
    <div className="card public-table-card">
      <div className="table-scroll">
        <table className="table-stack">
          <thead>
            <tr>
              <th>节点</th>
              <th>状态</th>
              <th>系统 / 地区</th>
              <th>CPU</th>
              <th>内存</th>
              <th>磁盘</th>
              <th>趋势</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => (
              <tr key={n.node_id}>
                <td data-label="节点">
                  <b>{n.remark || n.node_id}</b>
                  {n.hostname ? <div className="cell-sub">{n.hostname}</div> : null}
                </td>
                <td data-label="状态">
                  {n.online !== undefined ? (
                    <span className={`badge ${n.online ? 'online' : 'offline'}`}>{n.online ? '在线' : '离线'}</span>
                  ) : '-'}
                </td>
                <td data-label="系统/地区">
                  <div style={{ fontSize: 12 }}>
                    {[n.os_type, n.os_version, n.arch].filter(Boolean).join(' · ') || '-'}
                  </div>
                  {n.geo && (
                    <div className="cell-sub">
                      {[n.geo.country_code, n.geo.country, n.geo.region, n.geo.city].filter(Boolean).join(' ')}
                    </div>
                  )}
                </td>
                <td data-label="CPU">{n.cpu_usage !== undefined ? `${n.cpu_usage}%` : '-'}</td>
                <td data-label="内存">
                  {n.mem_usage !== undefined ? `${n.mem_usage}%` : '-'}
                  <div className="cell-sub">{bytesPair(n.mem_used_bytes, n.mem_total_bytes)}</div>
                </td>
                <td data-label="磁盘">
                  {n.disk_usage !== undefined ? `${n.disk_usage}%` : '-'}
                  <div className="cell-sub">{bytesPair(n.disk_used_bytes, n.disk_total_bytes)}</div>
                </td>
                <td data-label="趋势">
                  <button className="secondary btn-sm" onClick={() => nav(`/public/node/${n.node_id}`)}>查看</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function MiniBar({ label, value, sub }) {
  if (value === undefined || value === null) return null
  const v = Math.max(0, Math.min(100, value))
  const cls = v > 85 ? 'danger' : v > 60 ? 'warn' : ''
  return (
    <div className="public-mini">
      <div className="public-mini-row">
        <span className="k">{label}</span>
        <span className="v">{v.toFixed(1)}%</span>
      </div>
      <div className={`bar ${cls}`}><div style={{ width: `${v}%` }} /></div>
      {sub ? <div className="cell-sub">{sub}</div> : null}
    </div>
  )
}

// 节点详情页：展示公开字段明细 + 历史趋势（历史数据统计）
export function PublicNode() {
  const { nodeId } = useParams()
  const [searchParams, setSearchParams] = useSearchParams()
  const range = searchParams.get('range') || '24h'
  const [node, setNode] = useState(null)
  const [samples, setSamples] = useState([])
  const [loaded, setLoaded] = useState(false)
  const [missing, setMissing] = useState(false)

  // 节点明细：从公开状态接口里筛出该 node_id（接口不提供"按 node 查详情"，
  // 避免多出一个可枚举的详情端点）。
  useEffect(() => {
    let alive = true
    PublicApi.state()
      .then((d) => {
        if (!alive) return
        const found = (d.nodes || []).find((x) => x.node_id === nodeId)
        if (found) setNode(found)
        else setMissing(true)
      })
      .catch((err) => { if (alive && err.response?.status === 404) setMissing(true) })
    return () => { alive = false }
  }, [nodeId])

  useEffect(() => {
    let alive = true
    setLoaded(false)
    PublicApi.samples(nodeId, range)
      .then((d) => { if (alive) setSamples(Array.isArray(d.samples) ? d.samples : []) })
      .catch(() => { if (alive) setSamples([]) })
      .finally(() => { if (alive) setLoaded(true) })
    return () => { alive = false }
  }, [nodeId, range])

  const ranges = [
    { key: '1h', label: '最近 1 小时' },
    { key: '6h', label: '最近 6 小时' },
    { key: '24h', label: '最近 24 小时' },
    { key: '7d', label: '最近 7 天' },
  ]

  if (missing) {
    return (
      <div className="public-shell">
        <div className="public-empty">
          <div style={{ fontSize: 32, marginBottom: 8 }}>🔍</div>
          <div>该节点不存在或未开放展示。</div>
          <div style={{ marginTop: 16 }}><Link className="public-btn" to="/public">返回节点列表</Link></div>
        </div>
      </div>
    )
  }

  return (
    <div className="public-shell">
      <header className="public-head">
        <div>
          <div className="public-crumb"><Link to="/public">← 返回节点列表</Link></div>
          <h1>{node ? (node.remark || node.node_id) : '节点详情'}</h1>
          {node && (
            <div className="public-desc">
              {[node.os_type, node.os_version, node.arch].filter(Boolean).join(' · ') || node.node_id}
              {node.geo ? ` · ${[node.geo.country, node.geo.region, node.geo.city].filter(Boolean).join(' ')}` : ''}
            </div>
          )}
        </div>
        <div className="public-head-actions">
          <select
            value={range}
            onChange={(e) => setSearchParams({ range: e.target.value })}
            style={{ width: 150 }}
          >
            {ranges.map((r) => <option key={r.key} value={r.key}>{r.label}</option>)}
          </select>
        </div>
      </header>

      {node && <NodeFacts node={node} />}

      <div className="card">
        <h3>历史趋势</h3>
        {!loaded ? (
          <div className="public-empty">正在加载历史数据…</div>
        ) : (
          <PublicTrend samples={samples} />
        )}
      </div>
    </div>
  )
}

// 节点公开字段明细（只渲染存在的字段：服务端未开启的字段根本不返回）
function NodeFacts({ node }) {
  const rows = useMemo(() => {
    const out = []
    const push = (label, value) => {
      if (value === undefined || value === null || value === '' || value === 0) return
      out.push({ label, value })
    }
    push('节点编号', node.node_id)
    push('主机名', node.hostname)
    push('操作系统', [node.os_type, node.os_version].filter(Boolean).join(' '))
    push('内核版本', node.kernel_version)
    push('架构', node.arch)
    push('地区', node.geo ? [node.geo.country, node.geo.region, node.geo.city].filter(Boolean).join(' ') : '')
    push('公网 IP', node.public_ip)
    push('公网 IPv6', node.public_ipv6)
    push('CPU 型号', node.cpu_model)
    push('CPU 利用率', node.cpu_usage !== undefined ? `${node.cpu_usage}%` : '')
    push('内存', node.mem_usage !== undefined
      ? `${node.mem_usage}%（${bytesPair(node.mem_used_bytes, node.mem_total_bytes) || '—'}）`
      : '')
    push('磁盘', node.disk_usage !== undefined
      ? `${node.disk_usage}%（${bytesPair(node.disk_used_bytes, node.disk_total_bytes) || '—'}）`
      : '')
    push('运行时长', node.uptime ? fmtDuration(node.uptime) : '')
    push('最后在线', node.last_seen ? new Date(node.last_seen * 1000).toLocaleString() : '')
    return out
  }, [node])

  if (rows.length === 0) return null
  return (
    <div className="card">
      <h3>节点信息</h3>
      <div className="public-facts">
        {rows.map((r) => (
          <div className="public-fact" key={r.label}>
            <span className="k">{r.label}</span>
            <span className="v">{r.value}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

// 历史趋势折线图（轻量 SVG，无外部依赖；只画服务端返回的序列）
export function PublicTrend({ samples }) {
  const W = 900, H = 240, PAD = 34
  const series = (samples || []).slice(-300)
  if (series.length === 0) {
    return (
      <div className="public-empty">
        该时间段暂无历史采样数据（节点可能刚接入，或历史数据已过留存期）。
      </div>
    )
  }
  // 哪些序列可用：以第一条有值的采样为准
  const keys = [
    { key: 'cpu_usage', color: '#4da3ff', label: 'CPU' },
    { key: 'mem_usage', color: '#52c41a', label: '内存' },
    { key: 'disk_usage', color: '#fa8c16', label: '磁盘' },
  ].filter((k) => series.some((s) => typeof s[k.key] === 'number'))

  if (keys.length === 0) {
    return <div className="public-empty">当前配置未开放任何性能指标，历史趋势无法展示。</div>
  }

  const draw = (k) => {
    const vals = series.map((s) => (typeof s[k.key] === 'number' ? s[k.key] : null))
    const pts = []
    vals.forEach((v, i) => {
      if (v === null) return
      const x = PAD + (i / Math.max(1, series.length - 1)) * (W - PAD * 2)
      const y = H - PAD - (Math.max(0, Math.min(100, v)) / 100) * (H - PAD * 2)
      pts.push(`${x.toFixed(1)},${y.toFixed(1)}`)
    })
    return (
      <g key={k.key}>
        <polyline points={pts.join(' ')} fill="none" stroke={k.color} strokeWidth="2" />
      </g>
    )
  }

  const first = series[0]?.ts
  const last = series[series.length - 1]?.ts
  const fmtTs = (ts) => (ts ? new Date(ts * 1000).toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 'auto' }}>
        {[0, 25, 50, 75, 100].map((g) => {
          const y = H - PAD - (g / 100) * (H - PAD * 2)
          return (
            <g key={g}>
              <line x1={PAD} y1={y} x2={W - PAD} y2={y} stroke="#2a3340" strokeDasharray="4 4" />
              <text x={PAD - 6} y={y + 4} fill="#8b98a5" fontSize="10" textAnchor="end">{g}%</text>
            </g>
          )
        })}
        {keys.map(draw)}
        <text x={PAD} y={H - 8} fill="#8b98a5" fontSize="10">{fmtTs(first)}</text>
        <text x={W - PAD} y={H - 8} fill="#8b98a5" fontSize="10" textAnchor="end">{fmtTs(last)}</text>
        <g>
          {keys.map((k, i) => (
            <g key={k.key}>
              <rect x={PAD + i * 62} y={4} width={10} height={10} fill={k.color} />
              <text x={PAD + 14 + i * 62} y={13} fill="#8b98a5" fontSize="11">{k.label}</text>
            </g>
          ))}
        </g>
      </svg>
      <div className="cell-sub" style={{ marginTop: 6 }}>
        共 {series.length} 个采样点（服务端已按需降采样）
      </div>
    </div>
  )
}

function bytesPair(used, total) {
  if (!total || total <= 0) return ''
  return `${fmtBytes(used)}/${fmtBytes(total)}`
}

function fmtBytes(b) {
  const v = Number(b) || 0
  if (v <= 0) return '0G'
  if (v >= 1024 ** 4) return (v / 1024 ** 4).toFixed(2) + 'T'
  if (v >= 1024 ** 3) return (v / 1024 ** 3).toFixed(2) + 'G'
  if (v >= 1024 ** 2) return (v / 1024 ** 2).toFixed(1) + 'M'
  if (v >= 1024) return (v / 1024).toFixed(1) + 'K'
  return v + 'B'
}

function fmtDuration(s) {
  const v = Number(s) || 0
  if (v < 60) return `${Math.floor(v)}秒`
  if (v < 3600) return `${Math.floor(v / 60)}分`
  if (v < 86400) return `${Math.floor(v / 3600)}小时`
  return `${Math.floor(v / 86400)}天${Math.floor((v % 86400) / 3600)}小时`
}
