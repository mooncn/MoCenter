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
  const [keyword, setKeyword] = useState('')
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

  const nodes = snapshot?.nodes || []
  const config = snapshot?.config || {}

  // 搜索：纯前端过滤（公开页节点数有限，不额外增加接口与限流压力）
  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    if (!kw) return nodes
    return nodes.filter((n) => `${n.remark || ''} ${n.hostname || ''} ${n.node_id} ${geoText(n)}`
      .toLowerCase().includes(kw))
  }, [nodes, keyword])

  if (loading) {
    return (
      <div className="public-shell">
        <div className="public-skeleton">
          <div className="sk-bar" />
          <div className="sk-metrics">
            {Array.from({ length: 6 }).map((_, i) => <div className="sk-metric" key={i} />)}
          </div>
          <div className="sk-cards">
            {Array.from({ length: 3 }).map((_, i) => <div className="sk-card" key={i} />)}
          </div>
        </div>
      </div>
    )
  }
  if (disabled) {
    return (
      <div className="public-shell">
        <div className="public-empty">
          <div className="public-empty-icon">🔒</div>
          <div className="public-empty-title">本站未开放公开状态页</div>
          <div className="public-empty-sub">
            公开状态页需要在后台「展示配置」中开启总开关后才会生效。
          </div>
          <div className="public-empty-actions">
            <Link className="public-btn" to="/admin/login">登录管理后台</Link>
          </div>
        </div>
      </div>
    )
  }

  const summary = snapshot?.summary || {}
  const netEnabled = !!config.fields?.network
  return (
    <div className="public-shell">
      <header className="public-head">
        <div className="public-head-main">
          <h1>
            {config.title || 'MoCenter 节点状态'}
            <LiveDot />
          </h1>
          <div className="public-desc">{config.description}</div>
          <div className="public-head-meta">
            <span>更新于 {fmtClock(summary.server_time || Math.floor(Date.now() / 1000))}</span>
            {netEnabled && (
              <span title="流量累计口径">
                · 流量口径：{config.traffic_mode === 'window' ? '留存窗口内累计' : '自设备启动累计'}
              </span>
            )}
          </div>
        </div>
        <div className="public-head-actions">
          {config.hosts_enabled !== false && nodes.length > 0 && (
            <>
              <input
                className="public-search"
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
                placeholder="搜索备注 / 地区"
                aria-label="搜索节点"
              />
              <div className="seg">
                <button className={view === 'card' ? 'on' : ''} onClick={() => setView('card')}>卡片</button>
                <button className={view === 'list' ? 'on' : ''} onClick={() => setView('list')}>列表</button>
              </div>
            </>
          )}
          <Link className="public-btn ghost" to="/admin/login">登录后台</Link>
        </div>
      </header>

      <SummaryBar summary={summary} netEnabled={netEnabled} trafficMode={config.traffic_mode} />

      {config.hosts_enabled === false ? (
        <div className="public-empty">站点仅公开汇总指标，主机明细未开放。</div>
      ) : nodes.length === 0 ? (
        <div className="public-empty">当前没有可展示的节点。</div>
      ) : filtered.length === 0 ? (
        <div className="public-empty">没有匹配「{keyword}」的节点。</div>
      ) : (
        <>
          <div className="public-count">
            共 {filtered.length} 个节点{keyword ? `（已筛选，总 ${nodes.length} 个）` : ''}
          </div>
          {view === 'card'
            ? <NodeCards nodes={filtered} />
            : <NodeTable nodes={filtered} />}
        </>
      )}

      <footer className="public-foot">
        数据每 5 秒由 Agent 上报 · 页面实时刷新 · <Link to="/admin/login">管理后台</Link>
      </footer>
    </div>
  )
}

// 实时状态指示点：WS 推送到达时闪一下，让"页面是活的"这件事可见
function LiveDot() {
  return <span className="live-dot" title="实时刷新中" />
}

// 站点级汇总（不含任何主机标识）
function SummaryBar({ summary, netEnabled, trafficMode }) {
  const items = [
    { label: '节点总数', value: summary.total ?? 0 },
    { label: '在线', value: summary.online ?? 0, tone: 'ok' },
    { label: '离线', value: summary.offline ?? 0, tone: (summary.offline ?? 0) > 0 ? 'bad' : '' },
    { label: '平均 CPU', value: `${summary.avg_cpu ?? 0}%` },
    { label: '平均内存', value: `${summary.avg_mem ?? 0}%` },
    { label: '平均磁盘', value: `${summary.avg_disk ?? 0}%` },
  ]
  if (netEnabled) {
    items.push(
      { label: '下行速率', value: fmtRate(summary.net_rate_in), sub: '站点合计', tone: 'net' },
      { label: '上行速率', value: fmtRate(summary.net_rate_out), sub: '站点合计', tone: 'net' },
      {
        label: trafficMode === 'window' ? '窗口累计流量' : '累计流量',
        value: fmtBytes((summary.net_total_in || 0) + (summary.net_total_out || 0)),
        sub: `↓${fmtBytes(summary.net_total_in)} ↑${fmtBytes(summary.net_total_out)}`,
        tone: 'net',
        wide: true,
      },
    )
  }
  return (
    <div className="public-metrics">
      {items.map((it) => (
        <div className={`public-metric ${it.tone || ''} ${it.wide ? 'wide' : ''}`} key={it.label}>
          <div className="v">{it.value}</div>
          <div className="l">{it.label}</div>
          {it.sub ? <div className="s">{it.sub}</div> : null}
        </div>
      ))}
    </div>
  )
}

// 卡片视图（移动端友好）。
// 交互：整张卡片就是一个按钮，**点击直接进入历史趋势** —— 不再需要额外按钮，
// 卡片底部也不再挂"查看历史趋势"提示（多一个入口反而让卡片显得啰嗦）。
function NodeCards({ nodes }) {
  const nav = useNavigate()
  return (
    <div className="public-cards">
      {nodes.map((n) => (
        <button
          className={`public-card ${n.online ? '' : 'offline'}`}
          key={n.node_id}
          onClick={() => nav(`/public/node/${n.node_id}`)}
          title="点击查看历史趋势"
        >
          <div className="public-card-head">
            <span className="public-card-name">{n.remark || n.node_id}</span>
            <span className="public-card-head-right">
              {n.geo?.country_code && <Flag code={n.geo.country_code} />}
              {n.online !== undefined && (
                <span className={`badge ${n.online ? 'online' : 'offline'}`}>{n.online ? '在线' : '离线'}</span>
              )}
            </span>
          </div>
          <div className="public-card-meta">
            {n.os_type && <span>{n.os_type}{n.arch ? ` · ${n.arch}` : ''}</span>}
            {geoText(n) && <span className="public-geo">{geoText(n)}</span>}
          </div>
          <MiniBar label="CPU" value={n.cpu_usage} />
          <MiniBar label="内存" value={n.mem_usage} sub={bytesPair(n.mem_used_bytes, n.mem_total_bytes)} />
          <MiniBar label="磁盘" value={n.disk_usage} sub={bytesPair(n.disk_used_bytes, n.disk_total_bytes)} />
          {hasNet(n) && (
            <div className="public-net">
              <span className="down">↓ {fmtRate(n.net_rate_in)}</span>
              <span className="up">↑ {fmtRate(n.net_rate_out)}</span>
              <span className="total" title="累计流量">合计 {fmtBytes((n.net_total_in || 0) + (n.net_total_out || 0))}</span>
            </div>
          )}
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
              <th>网络</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => (
              <tr key={n.node_id} className="public-row" onClick={() => nav(`/public/node/${n.node_id}`)}>
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
                  {geoText(n) && (
                    <div className="cell-sub">
                      {n.geo?.country_code ? <Flag code={n.geo.country_code} /> : null} {geoText(n)}
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
                <td data-label="网络">
                  {hasNet(n) ? (
                    <>
                      <div style={{ color: 'var(--net-down)', fontSize: 12 }}>↓ {fmtRate(n.net_rate_in)}</div>
                      <div style={{ color: 'var(--net-up)', fontSize: 12 }}>↑ {fmtRate(n.net_rate_out)}</div>
                      <div className="cell-sub">合计 {fmtBytes((n.net_total_in || 0) + (n.net_total_out || 0))}</div>
                    </>
                  ) : '-'}
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

// 国旗：由 ISO 国家代码生成区域指示符 emoji（零依赖，不请求任何外部资源）。
// 放在备注右侧，让"这台机器在哪个国家"一眼可辨。
function Flag({ code }) {
  const flag = useMemo(() => countryFlag(code), [code])
  if (!flag) return null
  return <span className="public-flag" title={code}>{flag}</span>
}

// 国家代码 -> 国旗 emoji
function countryFlag(code) {
  if (!code || code.length !== 2) return ''
  const upper = code.toUpperCase()
  if (!/^[A-Z]{2}$/.test(upper)) return ''
  const base = 0x1F1E6
  const a = 'A'.charCodeAt(0)
  return String.fromCodePoint(base + (upper.charCodeAt(0) - a), base + (upper.charCodeAt(1) - a))
}

// 地区文本（国家 省 市），空字段自动跳过
function geoText(n) {
  if (!n?.geo) return ''
  return [n.geo.country, n.geo.region, n.geo.city].filter(Boolean).join(' ')
}

// 是否有可展示的网络数据
function hasNet(n) {
  return n.net_rate_in !== undefined || n.net_rate_out !== undefined
    || n.net_total_in !== undefined || n.net_total_out !== undefined
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
    const pull = () => PublicApi.state().then((d) => {
      if (!alive) return
      const found = (d.nodes || []).find((x) => x.node_id === nodeId)
      if (found) { setNode(found); setMissing(false) }
      else setMissing(true)
    }).catch((err) => { if (alive && err.response?.status === 404) setMissing(true) })

    pull()
    // 详情页同样需要"活的"：整页刷新时用轮询兜底（历史接口与 WS 推送彼此独立）
    const timer = setInterval(() => { if (!document.hidden) pull() }, 15000)
    return () => { alive = false; clearInterval(timer) }
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
          <div className="public-empty-icon">🔍</div>
          <div className="public-empty-title">该节点不存在或未开放展示</div>
          <div className="public-empty-actions">
            <Link className="public-btn" to="/public">返回节点列表</Link>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="public-shell">
      <header className="public-head">
        <div className="public-head-main">
          <div className="public-crumb"><Link to="/public">← 返回节点列表</Link></div>
          <h1>
            {node ? (node.remark || node.node_id) : '节点详情'}
            {node?.geo?.country_code ? <Flag code={node.geo.country_code} /> : null}
            {node?.online !== undefined && (
              <span className={`badge ${node.online ? 'online' : 'offline'}`} style={{ marginLeft: 8 }}>
                {node.online ? '在线' : '离线'}
              </span>
            )}
          </h1>
          <div className="public-desc">
            {node ? ([node.os_type, node.os_version, node.arch].filter(Boolean).join(' · ') || node.node_id) : ''}
            {node && geoText(node) ? ` · ${geoText(node)}` : ''}
          </div>
        </div>
        <div className="public-head-actions">
          <div className="seg">
            {ranges.map((r) => (
              <button
                key={r.key}
                className={range === r.key ? 'on' : ''}
                onClick={() => setSearchParams({ range: r.key })}
              >
                {r.label.replace('最近 ', '')}
              </button>
            ))}
          </div>
        </div>
      </header>

      {node && <NodeFacts node={node} />}

      <div className="card">
        <div className="public-card-title">
          <h3>历史趋势</h3>
          <span className="cell-sub">窗口：{ranges.find((r) => r.key === range)?.label}</span>
        </div>
        {!loaded
          ? <div className="public-empty">正在加载历史数据…</div>
          : <PublicTrend samples={samples} />}
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
    push('地区', geoText(node))
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
    push('下行速率', hasNet(node) ? fmtRate(node.net_rate_in) : '')
    push('上行速率', hasNet(node) ? fmtRate(node.net_rate_out) : '')
    push('累计下行', node.net_total_in ? fmtBytes(node.net_total_in) : '')
    push('累计上行', node.net_total_out ? fmtBytes(node.net_total_out) : '')
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

// 历史趋势折线图（轻量 SVG，无外部依赖；只画服务端返回的序列）。
//
// 两类序列共用一张图但在**两套纵轴**上：
//   - 百分比类（CPU / 内存 / 磁盘）走 0–100% 的左轴；
//   - 速率类（下行 / 上行）走自适应右轴，单位随量级切换。
// 混在一条轴里会把流量线压成贴地直线，所以分开。
export function PublicTrend({ samples }) {
  const W = 900, H = 260, PAD_L = 40, PAD_R = 62, PAD_T = 26, PAD_B = 26
  const series = (samples || []).slice(-300)
  if (series.length === 0) {
    return (
      <div className="public-empty">
        该时间段暂无历史采样数据（节点可能刚接入，或历史数据已过留存期）。
      </div>
    )
  }

  const pctKeys = [
    { key: 'cpu_usage', color: '#4da3ff', label: 'CPU', unit: '%' },
    { key: 'mem_usage', color: '#52c41a', label: '内存', unit: '%' },
    { key: 'disk_usage', color: '#fa8c16', label: '磁盘', unit: '%' },
  ].filter((k) => series.some((s) => typeof s[k.key] === 'number'))

  const netKeys = [
    { key: 'net_in_bps', color: '#21d4fd', label: '下行', unit: 'B/s' },
    { key: 'net_out_bps', color: '#f759ab', label: '上行', unit: 'B/s' },
  ].filter((k) => series.some((s) => typeof s[k.key] === 'number'))

  if (pctKeys.length === 0 && netKeys.length === 0) {
    return <div className="public-empty">当前配置未开放任何性能或流量指标，历史趋势无法展示。</div>
  }

  // 右轴上限：取速率序列的峰值并留 15% 余量，避免线贴顶
  let netMax = 0
  for (const k of netKeys) {
    for (const s of series) {
      if (typeof s[k.key] === 'number') netMax = Math.max(netMax, s[k.key])
    }
  }
  netMax = netMax > 0 ? netMax * 1.15 : 1

  const xOf = (i) => PAD_L + (i / Math.max(1, series.length - 1)) * (W - PAD_L - PAD_R)
  const yPct = (v) => H - PAD_B - (Math.max(0, Math.min(100, v)) / 100) * (H - PAD_T - PAD_B)
  const yNet = (v) => H - PAD_B - (Math.max(0, v) / netMax) * (H - PAD_T - PAD_B)

  // 折线分段：速率序列中间可能有"未知点"（服务端用 null 表示），
  // 直接连起来会画出一条假的下降线，因此按连续区间拆成多段。
  const polyline = (k, yFn) => {
    const segs = []
    let cur = []
    series.forEach((s, i) => {
      const v = s[k.key]
      if (typeof v === 'number') {
        cur.push(`${xOf(i).toFixed(1)},${yFn(v).toFixed(1)}`)
      } else if (cur.length) {
        segs.push(cur); cur = []
      }
    })
    if (cur.length) segs.push(cur)
    return segs.map((pts, idx) => (
      <polyline key={`${k.key}-${idx}`} points={pts.join(' ')} fill="none" stroke={k.color} strokeWidth="2"
        strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
    ))
  }

  const first = series[0]?.ts
  const last = series[series.length - 1]?.ts
  const fmtTs = (ts) => (ts ? new Date(ts * 1000).toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')
  const allKeys = [...pctKeys, ...netKeys]

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} className="public-trend-svg">
        <defs>
          {allKeys.map((k) => (
            <linearGradient key={k.key} id={`grad-${k.key}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={k.color} stopOpacity="0.28" />
              <stop offset="100%" stopColor={k.color} stopOpacity="0" />
            </linearGradient>
          ))}
        </defs>

        {/* 左轴：百分比网格 */}
        {[0, 25, 50, 75, 100].map((g) => (
          <g key={g}>
            <line x1={PAD_L} y1={yPct(g)} x2={W - PAD_R} y2={yPct(g)} stroke="#2a3340" strokeDasharray="4 4" />
            <text x={PAD_L - 6} y={yPct(g) + 4} fill="#8b98a5" fontSize="10" textAnchor="end">{g}%</text>
          </g>
        ))}

        {/* 右轴：速率刻度（与左轴同网格，便于对照） */}
        {netKeys.length > 0 && [0, 0.5, 1].map((f) => (
          <text key={f} x={W - PAD_R + 6} y={yNet(netMax * f) + 4} fill="#8b98a5" fontSize="10">
            {fmtRate(netMax * f)}
          </text>
        ))}

        {[...pctKeys.map((k) => polyline(k, yPct)), ...netKeys.map((k) => polyline(k, yNet))]}

        <text x={PAD_L} y={H - 8} fill="#8b98a5" fontSize="10">{fmtTs(first)}</text>
        <text x={W - PAD_R} y={H - 8} fill="#8b98a5" fontSize="10" textAnchor="end">{fmtTs(last)}</text>
      </svg>

      <div className="public-legend">
        {allKeys.map((k) => (
          <span key={k.key} className="public-legend-item">
            <i style={{ background: k.color }} />{k.label}
          </span>
        ))}
      </div>
      <div className="cell-sub" style={{ marginTop: 6 }}>
        共 {series.length} 个采样点（服务端已按需降采样）
        {netKeys.length > 0 ? ' · 流量为相邻采样差值，未知点不连线' : ''}
      </div>
    </div>
  )
}

// --- 格式化工具 ---

function bytesPair(used, total) {
  if (!total || total <= 0) return ''
  return `${fmtBytes(used)}/${fmtBytes(total)}`
}

// 字节 -> 可读容量（1024 进制）
function fmtBytes(b) {
  const v = Number(b) || 0
  if (v <= 0) return '0B'
  if (v >= 1024 ** 4) return (v / 1024 ** 4).toFixed(2) + 'T'
  if (v >= 1024 ** 3) return (v / 1024 ** 3).toFixed(2) + 'G'
  if (v >= 1024 ** 2) return (v / 1024 ** 2).toFixed(1) + 'M'
  if (v >= 1024) return (v / 1024).toFixed(1) + 'K'
  return v + 'B'
}

// 字节/秒 -> 可读速率；0 显示 0B/s，未知（undefined）显示 —
function fmtRate(bps) {
  if (bps === undefined || bps === null) return '—'
  const v = Number(bps) || 0
  if (v <= 0) return '0B/s'
  if (v >= 1024 ** 3) return (v / 1024 ** 3).toFixed(2) + 'GB/s'
  if (v >= 1024 ** 2) return (v / 1024 ** 2).toFixed(2) + 'MB/s'
  if (v >= 1024) return (v / 1024).toFixed(1) + 'KB/s'
  return v.toFixed(0) + 'B/s'
}

function fmtDuration(s) {
  const v = Number(s) || 0
  if (v < 60) return `${Math.floor(v)}秒`
  if (v < 3600) return `${Math.floor(v / 60)}分`
  if (v < 86400) return `${Math.floor(v / 3600)}小时`
  return `${Math.floor(v / 86400)}天${Math.floor((v % 86400) / 3600)}小时`
}

function fmtClock(ts) {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}
