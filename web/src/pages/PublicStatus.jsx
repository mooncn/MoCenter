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
  // 字段可见性：由服务端下发的 fields 开关驱动（等价于后台「展示配置 → 展示信息项」）。
  // 注意这里只用于**决定渲染哪些项**：未开启的字段在接口响应里本就不存在，
  // 所以即使这里判断错了，也不会泄露数据。
  const visible = (key) => !!config.fields?.[key]
  const netEnabled = visible('network')

  return (
    <div className="public-shell">
      {/* 顶部悬浮汇总条：页面滚动时始终可见（sticky），一眼能看到站点整体状态。
          按需求把节点统计从卡片区抽出来集中展示，卡片因此更小巧。 */}
      <SummaryBar
        summary={summary}
        netEnabled={netEnabled}
        trafficMode={config.traffic_mode}
        live
      />

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
            ? <NodeCards nodes={filtered} visible={visible} />
            : <NodeTable nodes={filtered} visible={visible} />}
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

// 站点级汇总（不含任何主机标识）—— 顶部悬浮指标条。
//
// 内容固定为运维最关心的六项：节点总数 / 在线 / 离线 / 上行速率 / 下行速率 / 累计流量。
// 「平均 CPU / 内存 / 磁盘」不在条上展示：这三项按主机逐台看才有意义，
// 站点均值反而容易掩盖问题主机（卡片上本来就有逐台数值）。
function SummaryBar({ summary, netEnabled, trafficMode }) {
  const offline = summary.offline ?? 0
  const items = [
    { key: 'total', label: '节点总数', value: summary.total ?? 0, unit: '台' },
    { key: 'online', label: '在线节点', value: summary.online ?? 0, unit: '台', tone: 'ok' },
    { key: 'offline', label: '离线节点', value: offline, unit: '台', tone: offline > 0 ? 'bad' : '' },
  ]
  if (netEnabled) {
    items.push(
      { key: 'up', label: '上行总网速', value: fmtRate(summary.net_rate_out), tone: 'up' },
      { key: 'down', label: '下行总网速', value: fmtRate(summary.net_rate_in), tone: 'down' },
      {
        key: 'traffic',
        label: trafficMode === 'window' ? '累计流量 · 留存窗口' : '累计流量 · 自启动',
        value: fmtBytes((summary.net_total_in || 0) + (summary.net_total_out || 0)),
        sub: `↓ ${fmtBytes(summary.net_total_in)} · ↑ ${fmtBytes(summary.net_total_out)}`,
      },
    )
  }
  return (
    <div className="public-summary" role="status" aria-label="站点汇总">
      <div className="public-summary-inner">
        {items.map((it) => (
          <div className={`sm-item ${it.tone || ''}`} key={it.key}>
            <span className="sm-v">{it.value}<i className="sm-u">{it.unit || ''}</i></span>
            <span className="sm-l">{it.label}</span>
            {it.sub ? <span className="sm-s">{it.sub}</span> : null}
          </div>
        ))}
      </div>
    </div>
  )
}

// 卡片视图（移动端友好）。
//
// 按需求精简为六项：名称（+国旗/状态点）/ 系统 / CPU / RAM / 磁盘 / 负载。
// 站点的整体统计移到顶部悬浮条，流量移到列表视图 —— 卡片只回答"这台机器现在怎么样"，
// 因此尺寸可以做小（<= 236px 宽即可排两列），一屏能看到更多机器。
//
// 交互：整张卡片就是按钮，点按直接进入历史趋势。
function NodeCards({ nodes, visible }) {
  const nav = useNavigate()
  const osOn = visible('os_type')
  const cpuOn = visible('cpu_usage')
  const memOn = visible('mem_usage')
  const diskOn = visible('disk_usage')
  const loadOn = visible('load')
  return (
    <div className="public-cards">
      {nodes.map((n) => {
        const hasMeta = osOn && (n.os_type || n.arch)
        const hasBars = cpuOn || memOn || diskOn || loadOn
        return (
          <button
            className={`public-card ${n.online === false ? 'offline' : ''}`}
            key={n.node_id}
            onClick={() => nav(`/public/node/${n.node_id}`)}
            title="点击查看历史趋势"
          >
            <div className="public-card-head">
              <span className="public-card-name" title={n.remark || n.node_id}>
                {visible('geo') && n.geo?.country_code ? <Flag code={n.geo.country_code} /> : null}
                {n.remark || n.node_id}
              </span>
              <span className="public-card-head-right">
                {n.online !== undefined && (
                  <span className={`dot ${n.online ? 'online' : 'offline'}`} title={n.online ? '在线' : '离线'} />
                )}
              </span>
            </div>

            {hasMeta && (
              <div className="public-card-meta">
                <span className="tag-os">{osText(n)}</span>
                {visible('geo') && geoText(n) && <span className="tag-geo">{geoText(n)}</span>}
              </div>
            )}

            {hasBars && (
              <div className="public-card-bars">
                {cpuOn && <MiniBar label="CPU" value={n.cpu_usage} />}
                {memOn && <MiniBar label="RAM" value={n.mem_usage} />}
                {diskOn && <MiniBar label="磁盘" value={n.disk_usage} />}
                {loadOn && <LoadLine node={n} />}
              </div>
            )}
          </button>
        )
      })}
    </div>
  )
}

// 负载一行：Load1 为主，括号内带 5/15 分钟值；服务端未下发时显示「—」。
function LoadLine({ node }) {
  const has = typeof node.load1 === 'number'
  const busy = has && node.load1 >= 1
  return (
    <div className="public-mini public-load">
      <div className="public-mini-row">
        <span className="k">负载</span>
        <span className={`v ${busy ? 'warn' : ''}`}>
          {has ? node.load1.toFixed(2) : '—'}
          {has && typeof node.load5 === 'number' && (
            <em className="load-more">{node.load5.toFixed(2)} / {(node.load15 ?? 0).toFixed(2)}</em>
          )}
        </span>
      </div>
    </div>
  )
}

// 列表视图（信息密度高）。
//
// 列：节点名称 / 状态 / 系统类型 / 系统版本 / 地区 / CPU / 内存 / 磁盘 / 网速 / 流量 / 负载。
// 每一列都受后台「展示配置 → 展示信息项」门控：未开启的列不渲染，
// 服务端也不会下发对应字段（双重保证）。
//
// 排序：只允许按**非动态字段**排序（名称/系统/地区/磁盘容量等），
// 因为这些值在一次快照内是稳定的；按 CPU/网速等每 5 秒变化的字段排序会让
// 行序不停跳动，反而看不清。默认「在线优先 + 名称」。
function NodeTable({ nodes, visible }) {
  const nav = useNavigate()
  // sortKey='default' 表示"在线优先 + 名称"；其余为列 key。
  // sortDir 只在可排序列上生效：第一次点击升序，再点切换降序，第三次恢复默认。
  const [sortKey, setSortKey] = useState('default')
  const [sortDir, setSortDir] = useState('asc')
  const clickSort = (key) => {
    if (!SORTABLE.has(key)) return
    if (sortKey !== key) { setSortKey(key); setSortDir('asc'); return }
    if (sortDir === 'asc') { setSortDir('desc'); return }
    setSortKey('default'); setSortDir('asc')
  }

  const columns = useMemo(() => [
    visible('remark') || visible('hostname') ? { key: 'name', label: '节点名称' } : null,
    visible('online') ? { key: 'online', label: '状态' } : null,
    visible('os_type') ? { key: 'os', label: '系统类型' } : null,
    visible('os_version') ? { key: 'osver', label: '系统版本' } : null,
    visible('geo') ? { key: 'geo', label: '地区' } : null,
    visible('cpu_usage') ? { key: 'cpu', label: 'CPU' } : null,
    visible('mem_usage') ? { key: 'mem', label: '内存' } : null,
    visible('disk_usage') ? { key: 'disk', label: '磁盘' } : null,
    visible('network') ? { key: 'rate', label: '网速' } : null,
    visible('network') ? { key: 'traffic', label: '流量' } : null,
    visible('load') ? { key: 'load', label: '负载' } : null,
  ].filter(Boolean), [visible])

  const sorted = useMemo(() => sortNodes(nodes, sortKey, sortDir), [nodes, sortKey, sortDir])

  return (
    <div className="card public-table-card">
      <div className="table-scroll">
        <table className="table-stack public-table">
          <thead>
            <tr>
              {columns.map((c) => (
                <th key={c.key} className={SORTABLE.has(c.key) ? 'sortable' : ''}
                  onClick={() => clickSort(c.key)}
                  title={SORTABLE.has(c.key) ? '点击排序 / 再点切换升降序 / 第三次恢复默认（仅非动态字段）' : undefined}
                >
                  {c.label}
                  {SORTABLE.has(c.key) && (
                    <span className={`sort-mark ${sortKey === c.key ? 'on' : ''}`}>
                      {sortKey === c.key ? (sortDir === 'asc' ? '▲' : '▼') : '↕'}
                    </span>
                  )}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {sorted.map((n) => (
              <tr key={n.node_id} className="public-row" onClick={() => nav(`/public/node/${n.node_id}`)}>
                {columns.map((c) => <td key={c.key} data-label={c.label}>{cell(c.key, n, visible)}</td>)}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {sortKey !== 'default' && (
        <div className="cell-sub sort-hint">
          已按「{columns.find((c) => c.key === sortKey)?.label}」{sortDir === 'asc' ? '升序' : '降序'}排列 ·
          再点一次切换方向，第三次恢复默认（在线优先）
        </div>
      )}
    </div>
  )
}

// SORTABLE 允许排序的列：全部是**非动态字段**（不随探针周期变化）。
const SORTABLE = new Set(['name', 'online', 'os', 'osver', 'geo', 'disk'])

// sortNodes 按非动态字段排序；default = 在线优先 + 名称。
//
// dir='asc' 升序 / 'desc' 降序。每种排序都以「名称」作为并列时的次级键，
// 保证同值行的顺序稳定（不会随服务端 map 遍历顺序抖动）。
function sortNodes(list, key, dir = 'asc') {
  const arr = [...list]
  const sign = dir === 'desc' ? -1 : 1
  const byName = (a, b) => nameOf(a).localeCompare(nameOf(b), 'zh-Hans-CN')
  if (key === 'default') {
    return arr.sort((a, b) => {
      if (a.online !== b.online) return a.online ? -1 : 1
      return byName(a, b)
    })
  }
  const cmp = {
    name: byName,
    // 在线始终排在前面（"谁在线"是这张表的第一信息），方向只影响名称次序
    online: (a, b) => (Number(b.online) || 0) - (Number(a.online) || 0) || sign * byName(a, b),
    os: (a, b) => sign * (String(a.os_type || '').localeCompare(String(b.os_type || '')) || 0) || byName(a, b),
    osver: (a, b) => sign * (String(a.os_version || '').localeCompare(String(b.os_version || '')) || 0) || byName(a, b),
    geo: (a, b) => sign * (geoText(a).localeCompare(geoText(b), 'zh-Hans-CN') || 0) || byName(a, b),
    // 磁盘按**容量**排序比按百分比更有意义（直接看出谁是盘小的机器）；
    // 升序 = 容量小的在前，降序 = 容量大的在前。
    disk: (a, b) => sign * ((a.disk_total_bytes || 0) - (b.disk_total_bytes || 0)) || byName(a, b),
  }
  const fn = key === 'name' ? ((a, b) => sign * byName(a, b)) : cmp[key]
  return arr.sort(fn || byName)
}

// nameOf 排序用显示名（备注优先，其次主机名，最后节点编号）。
function nameOf(n) {
  return n.remark || n.hostname || n.node_id || ''
}

// cell 渲染单列内容。所有取值都做缺省处理：字段未开启时显示「-」而不是空白。
function cell(key, n, visible) {
  switch (key) {
    case 'name':
      return (
        <>
          <b className="row-name">
            {visible('geo') && n.geo?.country_code ? <Flag code={n.geo.country_code} /> : null}
            {n.remark || n.hostname || n.node_id}
          </b>
          {visible('node_id') && n.remark && n.node_id ? <div className="cell-sub">{n.node_id}</div> : null}
        </>
      )
    case 'online':
      return n.online === undefined
        ? '-'
        : <span className={`badge ${n.online ? 'online' : 'offline'}`}>{n.online ? '在线' : '离线'}</span>
    case 'os':
      return <span className="tag-os">{osText(n) || '-'}</span>
    case 'osver':
      return <span title={n.os_version || ''}>{n.os_version || '-'}</span>
    case 'geo':
      return geoText(n) || '-'
    case 'cpu':
      return n.cpu_usage === undefined ? '-' : <Pct value={n.cpu_usage} />
    case 'mem':
      return n.mem_usage === undefined ? '-' : (
        <>
          <Pct value={n.mem_usage} />
          <div className="cell-sub">{bytesPair(n.mem_used_bytes, n.mem_total_bytes)}</div>
        </>
      )
    case 'disk':
      return n.disk_usage === undefined ? '-' : (
        <>
          <Pct value={n.disk_usage} />
          <div className="cell-sub">{bytesPair(n.disk_used_bytes, n.disk_total_bytes)}</div>
        </>
      )
    case 'rate':
      if (!hasNet(n)) return '-'
      return (
        <>
          <div className="rate-down">↓ {fmtRate(n.net_rate_in)}</div>
          <div className="rate-up">↑ {fmtRate(n.net_rate_out)}</div>
        </>
      )
    case 'traffic':
      if (!hasTraffic(n)) return '-'
      return <span title={`↓ ${fmtBytes(n.net_total_in)} / ↑ ${fmtBytes(n.net_total_out)}`}>
        {fmtBytes((n.net_total_in || 0) + (n.net_total_out || 0))}
      </span>
    case 'load':
      if (typeof n.load1 !== 'number') return '-'
      return (
        <span title="1 / 5 / 15 分钟平均负载">
          {n.load1.toFixed(2)}
          <div className="cell-sub">{(n.load5 ?? 0).toFixed(2)} / {(n.load15 ?? 0).toFixed(2)}</div>
        </span>
      )
    default:
      return '-'
  }
}

// Pct 百分比文本（>85 标红，>60 标黄，与卡片进度条同口径）。
function Pct({ value }) {
  const v = Number(value) || 0
  const cls = v > 85 ? 'danger' : v > 60 ? 'warn' : ''
  return <span className={`pct ${cls}`}>{v.toFixed(1)}%</span>
}

// osText 系统类型文本：类型 + 架构（如 linux · amd64）。
function osText(n) {
  return [n.os_type, n.arch].filter(Boolean).join(' · ')
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

// 是否有可展示的网络速率数据
function hasNet(n) {
  return n.net_rate_in !== undefined || n.net_rate_out !== undefined
    || n.net_total_in !== undefined || n.net_total_out !== undefined
}

// 是否有可展示的累计流量（速率有值但累计未知时不该显示成 0B）
function hasTraffic(n) {
  return typeof n.net_total_in === 'number' || typeof n.net_total_out === 'number'
}

// 负载文本：服务端未下发（平台不提供）时返回空串
function loadText(n) {
  if (typeof n?.load1 !== 'number') return ''
  const parts = [n.load1, n.load5, n.load15].filter((v) => typeof v === 'number')
  return parts.map((v) => v.toFixed(2)).join(' / ')
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
            {node && loadText(node) ? ` · 负载 ${loadText(node)}` : ''}
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
    push('系统负载', loadText(node) ? `${loadText(node)}（1 / 5 / 15 分钟）` : '')
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
