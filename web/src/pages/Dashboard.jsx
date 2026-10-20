import React, { useState, useEffect, useMemo, useRef } from 'react'
import { Api } from '../api/api.js'
import Flag from '../components/Flag.jsx'
import Pagination from '../components/Pagination.jsx'

// ============================================================
// 排序（按列点击表头）
// ============================================================
//
// 关键设计：**只有"稳定字段"参与排序**。
// 主机列表每 5 秒被 WebSocket 整表替换一次，若按 CPU/内存/网速这类每帧都变的
// 数值排序，行序会一轮一个样 —— 用户刚想点某个主机的"查看"，它已经被挤走了。
// 因此：
//   · 可排序列只包含在一次刷新内不会变的字段（名称/状态/系统/架构/位置/磁盘容量/版本/最后在线）；
//   · 其他列点上去给出提示，而不是"点了没反应"；
//   · 每种排序都以「名称」作为并列时的二级键，保证同值行顺序确定（不会因遍历顺序抖动）。
const SORT_COLUMNS = {
  host: 'name',
  status: 'online',
  geo: 'geo',
  os: 'os',
  arch: 'arch',
  disk: 'disk',
  version: 'version',
  lastseen: 'lastseen',
}

// 动态列：点了会提示"不参与排序"，避免用户以为表头坏了
const DYNAMIC_COLUMNS = new Set(['publicip', 'cpu', 'mem', 'network', 'latency'])

// 显示名：备注优先，其次主机名，最后 agent_id
function nameOf(a) {
  return (a.remark || a.hostname || a.agent_id || '')
}

function lastSeenTs(a) {
  // 在线的用 last_seen；离线的用"最后一次离线时间"，缺失时退回 last_seen
  return Number(a.online ? a.last_seen : (a.last_offline_at || a.last_seen)) || 0
}

function geoOf(a) {
  const g = a.geo
  if (!g) return ''
  return [g.country, g.region, g.city].filter(Boolean).join(' ')
}

// sortAgents 返回排好序的**新数组**（不改原数组：agents 来自上层 state，
// 原地排序会让 React 看不出变化，也会影响其它正在读它的组件）。
function sortAgents(list, key, dir) {
  const arr = [...(list || [])]
  const sign = dir === 'desc' ? -1 : 1
  const byName = (a, b) => nameOf(a).localeCompare(nameOf(b), 'zh-Hans-CN', { numeric: true })
  if (!key || key === 'default') {
    // 默认：在线优先 + 名称。在线是这张表的第一信息，不应被排序方向打乱。
    return arr.sort((a, b) => (a.online === b.online ? byName(a, b) : (a.online ? -1 : 1)))
  }
  const cmp = {
    name: (a, b) => sign * byName(a, b),
    online: (a, b) => (Number(b.online) || 0) - (Number(a.online) || 0) || sign * byName(a, b),
    geo: (a, b) => sign * (geoOf(a).localeCompare(geoOf(b), 'zh-Hans-CN') || 0) || byName(a, b),
    os: (a, b) => sign * (String(a.os_type || '').localeCompare(String(b.os_type || '')) || 0) || byName(a, b),
    arch: (a, b) => sign * (String(a.arch || '').localeCompare(String(b.arch || '')) || 0) || byName(a, b),
    // 磁盘按**容量**排（比按百分比有意义：一眼看出谁的盘小）
    disk: (a, b) => sign * ((a.disk_total_bytes || 0) - (b.disk_total_bytes || 0)) || byName(a, b),
    version: (a, b) => sign * (String(a.agent_version || '').localeCompare(String(b.agent_version || '')) || 0) || byName(a, b),
    lastseen: (a, b) => sign * (lastSeenTs(a) - lastSeenTs(b)) || byName(a, b),
  }
  return arr.sort(cmp[key] || byName)
}

// useSort 三态排序：未排序 -> 升序 -> 降序 -> 回到默认。
// 相比"升/降两态"，多出来的"取消排序"能让用户随时退回「在线优先」这个最实用的视图。
function useSort() {
  const [sortKey, setSortKey] = useState('default')
  const [sortDir, setSortDir] = useState('asc')
  const [hint, setHint] = useState('')
  const hintTimer = useRef(null)

  const click = (col) => {
    if (DYNAMIC_COLUMNS.has(col)) {
      showHint('该列每 5 秒都在变，排序会让行序不停跳动，因此不参与排序；请按「状态 / 主机 / 磁盘 / 最后在线」排序')
      return
    }
    const key = SORT_COLUMNS[col]
    if (!key) return
    if (sortKey !== key) { setSortKey(key); setSortDir('asc'); return }
    if (sortDir === 'asc') { setSortDir('desc'); return }
    setSortKey('default'); setSortDir('asc')
  }

  function showHint(msg) {
    setHint(msg)
    if (hintTimer.current) clearTimeout(hintTimer.current)
    // 3 秒后自动消失：它只是"为什么点不动"的解释，不是常驻提示
    hintTimer.current = setTimeout(() => setHint(''), 3000)
  }

  return { sortKey, sortDir, click, hint }
}

// SortMark 表头排序标记：未排序 ↕ / 升序 ▲ / 降序 ▼
function SortMark({ active, dir }) {
  return <span className={`sort-mark ${active ? 'on' : ''}`}>{active ? (dir === 'asc' ? '▲' : '▼') : '↕'}</span>
}

// TH_LABELS：排序列 key -> 显示名（用于排序状态提示条）
const TH_LABELS = {
  name: '主机', online: '状态', geo: '地理位置', os: '系统', arch: '架构',
  disk: '磁盘', version: '版本', lastseen: '最后在线',
}
// 反向索引：key -> 表头 col（"重置"按钮需要用它走回 clickSort 的三态逻辑）
const SORT_KEYS_BY_KEY = Object.fromEntries(Object.entries(SORT_COLUMNS).map(([col, key]) => [key, col]))

// Th 表头单元格。
//
// 移动端特殊处理：窄屏下表格会被 `.table-stack` 折叠成"一主机一卡片"，
// 表头整行 `display:none` —— 排序入口会**彻底消失**。因此这里额外渲染一个
// 只在小屏显示的排序按钮（`.th-mobile`），让手机上也能排序。
function Th({ col, label, sortKey, dir, onClick }) {
  const sortable = !!SORT_COLUMNS[col]
  const active = sortable && sortKey === SORT_COLUMNS[col]
  return (
    <th
      className={sortable ? 'sortable' : ''}
      onClick={sortable ? () => onClick(col) : undefined}
      title={sortable
        ? '点击排序 / 再点切换升降序 / 第三次恢复默认（仅非动态字段）'
        : '该列数值每 5 秒变化，不参与排序'}
    >
      <span className="th-label">{label}{sortable && <SortMark active={active} dir={dir} />}</span>
      {sortable && (
        <button
          type="button"
          className="th-mobile"
          onClick={(e) => { e.stopPropagation(); onClick(col) }}
          aria-label={`按${label}排序`}
        >{label}{<SortMark active={active} dir={dir} />}</button>
      )}
    </th>
  )
}

// 主机探针信息面板：展示各主机 CPU/内存/磁盘/流量等实时状态与历史趋势
export default function Dashboard({ agents }) {
  const [since, setSince] = useState('24h')
  const [selectedAgent, setSelectedAgent] = useState('')
  const [history, setHistory] = useState(null)
  // 服务端存活状态：null=检测中 true=在线 false=离线
  const [serverAlive, setServerAlive] = useState(null)

  // 列排序：状态保存在本组件，数据每 5 秒刷新也不会重置用户的选择
  const { sortKey, sortDir, click: clickSort, hint: sortHint } = useSort()

  // 分页：主机数会一直增长，整表渲染既拖慢首屏也让移动端滚不到底。
  // 分页放在**排序之后**（先排全量再切页），否则"按磁盘容量排序"只会对当前页生效。
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const online = (agents || []).filter((a) => a.online).length
  const offline = (agents || []).filter((a) => !a.online).length
  // 资源占用均值只统计**在线**主机：离线主机的 CPU/内存是它们断线前最后一次上报的值，
  // 混进平均值会长期污染统计（一台机器下线后其旧值会永远留在分母里）。
  const onlineAgents = (agents || []).filter((a) => a.online)
  const avgOf = (key) => {
    if (onlineAgents.length === 0) return 0
    return onlineAgents.reduce((s, a) => s + (a[key] || 0), 0) / onlineAgents.length
  }
  // 保留一位小数：否则 0.4% 会被四舍五入成 0%，看起来像"没数据"
  const fmtPct = (v) => (v > 0 && v < 1 ? v.toFixed(1) : Math.round(v))

  // 排序结果做缓存：WebSocket 每 5 秒推一次新数组，但排序键不变时不重复排
  const sortedAgents = useMemo(() => sortAgents(agents, sortKey, sortDir), [agents, sortKey, sortDir])

  // 当前页切片。页码收敛在渲染期算（而不是用 effect 改 state）：
  // 主机数变少或每页条数调大时，越界的页码当帧即被夹回最后一页，
  // 不会先渲染一屏空白再跳回。
  const totalPages = Math.max(1, Math.ceil(sortedAgents.length / pageSize))
  const curPage = Math.min(Math.max(1, page), totalPages)
  const pagedAgents = useMemo(
    () => sortedAgents.slice((curPage - 1) * pageSize, curPage * pageSize),
    [sortedAgents, curPage, pageSize]
  )
  const changePageSize = (n) => { setPageSize(n); setPage(1) }

  const sinceMap = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800 }

  const removeAgent = async (a) => {
    const name = nameOf(a)
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
      if (selectedAgent === a.agent_id) setSelectedAgent('')
      window.dispatchEvent(new Event('mc-agents-refresh'))
    } catch (e) {
      alert('删除失败：' + (e.response?.data?.msg || e.message))
    }
  }

  // 每 5 秒探测服务端存活状态（/api/health 无需鉴权）
  useEffect(() => {
    let alive = true
    const check = async () => {
      try { await Api.health(); alive && setServerAlive(true) }
      catch { alive && setServerAlive(false) }
    }
    check()
    const t = setInterval(check, 5000)
    return () => { alive = false; clearInterval(t) }
  }, [])

  // 加载选中主机的历史探针趋势
  useEffect(() => {
    if (!selectedAgent) {
      setHistory(null)
      return
    }
    const sinceTs = new Date(Date.now() - sinceMap[since] * 1000).toISOString()
    // max=300：请求服务端降采样，避免长时间跨度返回海量采样点
    Api.history(selectedAgent, sinceTs, 300)
      .then((data) => setHistory(data))
      .catch(() => setHistory([]))
  }, [selectedAgent, since])

  return (
    <div>
      {/* 顶部浮动统计栏：滚动时吸附在顶部。右侧「回首页」是公开状态页入口 ——
          这里已经是后台仪表盘，再放一个「登录后台」只会让人误以为当前没登录。 */}
      <div className="stat-bar">
        <div className="stat-bar-head">
          <h2>仪表盘</h2>
          {/* 站点汇总与下面「主机探针信息总览」同源同口径，保持一行摘要 */}
          <div className="sub">主机探针信息总览 · 在线 {online} / 离线 {offline} / 共 {agents.length}</div>
        </div>
        <span className={`badge ${serverAlive === false ? 'offline' : serverAlive ? 'online' : ''}`}>
          <span className={`dot ${serverAlive === false ? 'offline' : serverAlive ? 'online' : ''}`} />{' '}
          {serverAlive === null ? '服务端检测中…' : serverAlive ? '服务端在线' : '服务端离线'}
        </span>
        <select value={since} onChange={(e) => setSince(e.target.value)} style={{ width: 150 }}>
          <option value="1h">最近 1 小时</option>
          <option value="6h">最近 6 小时</option>
          <option value="24h">最近 24 小时</option>
          <option value="7d">最近 7 天</option>
        </select>
        {/* 回首页：公开节点状态页（未开放时该页会给出提示） */}
        <a className="btn-login btn-home" href="/" title="返回公开节点状态页">🏠 回首页</a>
      </div>

      {/* 主机列表 */}
      <div className="card">
        <div className="card-head-row">
          <h3>主机探针信息总览</h3>
          {/* 五项汇总指标：原先各占一张卡片，独立于列表之上。
              移动端一屏只能放两列，五张卡就把首屏占满了 —— 这里收进总览卡片头部，
              列表本身立刻上移，桌面端也不再重复一块统计区。 */}
          <div className="overview-metrics">
            <OverviewMetric value={online} label="在线主机" tone="ok" />
            <OverviewMetric value={offline} label="离线主机" tone={offline > 0 ? 'bad' : ''} />
            <OverviewMetric value={agents.length} label="注册主机" />
            <OverviewMetric value={`${fmtPct(avgOf('cpu_usage'))}%`} label="平均 CPU" sub={`在线 ${onlineAgents.length} 台`} />
            <OverviewMetric value={`${fmtPct(avgOf('mem_usage'))}%`} label="平均内存" sub={`在线 ${onlineAgents.length} 台`} />
          </div>
        </div>
        {/* 排序提示条：点动态列时解释"为什么点不动"，3 秒后自动消失 */}
        {sortHint ? <div className="notice" style={{ background: 'var(--panel-2)', color: 'var(--muted)' }}>{sortHint}</div> : null}
        {sortKey !== 'default' && (
          <div className="cell-sub" style={{ marginBottom: 8 }}>
            已按「{TH_LABELS[sortKey] || sortKey}」{sortDir === 'asc' ? '升序' : '降序'}排列 ·
            再点一次切换方向，第三次恢复默认（在线优先）
            <button
              className="btn-mini"
              style={{ marginLeft: 8, fontSize: 11, padding: '2px 8px' }}
              onClick={() => clickSort(SORT_KEYS_BY_KEY[sortKey])}
              title="恢复默认排序（在线优先 + 名称）"
            >重置</button>
          </div>
        )}
        <div className="table-scroll">
        <table className="table-stack sortable-table blocky">
          <thead>
            <tr>
              <Th col="host" label="主机" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="status" label="状态" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="geo" label="公网 IP / 地理位置" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="os" label="系统" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="arch" label="架构" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="cpu" label="CPU" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="mem" label="内存" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="disk" label="磁盘" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="network" label="网络(累计↓↑ / 速率↓↑)" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="latency" label="延迟" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="version" label="版本" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <Th col="lastseen" label="最后在线" sortKey={sortKey} dir={sortDir} onClick={clickSort} />
              <th>趋势</th>
            </tr>
          </thead>
          <tbody>
            {pagedAgents.map((a) => {
              const trendOpen = selectedAgent === a.agent_id
              return (
              <React.Fragment key={a.agent_id}>
              {/* 离线节点整行标红：红点位于卡片右上角（与在线绿点同一位置） */}
              <tr className={a.online ? '' : 'row-offline'}>
                <td data-label="主机">
                  <b>{a.remark || a.hostname}</b>
                  <div style={{ color: '#8b98a5', fontSize: 12 }}>
                    {a.hostname}
                    {a.remark ? ' · ' + a.agent_id : ''}
                  </div>
                  {a.uuid ? (
                    <div
                      style={{ color: '#6b7280', fontSize: 10, fontFamily: 'Menlo, Consolas, monospace', maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                      title={`设备唯一 UUID：${a.uuid}`}
                    >
                      UUID: {a.uuid}
                    </div>
                  ) : null}
                  <RemarkEditor agent={a} onSaved={() => window.dispatchEvent(new Event('mc-remark-saved'))} />
                </td>
                <td data-label="状态">
                  <span className={`badge ${a.online ? 'online' : 'offline'}`}>
                    {a.online ? '在线' : '离线'}
                  </span>
                  <span className={`cell-dot ${a.online ? 'online' : 'offline'}`} aria-hidden="true" />
                </td>
                <td data-label="公网IP/位置">
                  <PublicIPCell agent={a} />
                </td>
                <td data-label="系统">
                  <div style={{ fontWeight: 600, fontSize: 12 }}>{a.os_version || a.os_type}</div>
                  <div style={{ color: '#8b98a5', fontSize: 11 }}>{a.os_type} · {a.arch}</div>
                </td>
                <td data-label="架构">{a.arch}</td>
                <td data-label="CPU">
                  <div style={{ fontSize: 11, maxWidth: 210, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={(a.cpu_model || '') + (a.cpu_mhz ? ` @ ${(a.cpu_mhz / 1000).toFixed(2)}GHz` : '')}>
                    {a.cpu_model || '-'}{a.cpu_mhz ? ` @ ${(a.cpu_mhz / 1000).toFixed(2)}GHz` : ''}
                  </div>
                  <CpuBar v={a.cpu_usage} />
                </td>
                <td data-label="内存">
                  <div style={{ fontSize: 11 }}>{fmtGBytes(a.mem_used_bytes)}/{fmtGBytes(a.mem_total_bytes)}</div>
                  <MemBar v={a.mem_usage} />
                </td>
                <td data-label="磁盘">
                  <div style={{ fontSize: 11 }}>{fmtGBytes(a.disk_used_bytes)}/{fmtGBytes(a.disk_total_bytes)}</div>
                  <Bar v={a.disk_usage} />
                </td>
                <td data-label="网络" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                  <div style={{ color: '#4da3ff' }} title="网卡累计下行">↓ {fmtGB(a.net_in_bytes)}</div>
                  <div style={{ color: '#fa8c16' }} title="网卡累计上行">↑ {fmtGB(a.net_out_bytes)}</div>
                  <div style={{ color: '#52c41a', fontSize: 10 }}>
                    {fmtSpeed(a.net_rate_in)} / {fmtSpeed(a.net_rate_out)}
                  </div>
                </td>
                <td data-label="延迟">
                  <LatencyCell agent={a} />
                </td>
                <td data-label="版本">
                  <AgentVersionCell agent={a} />
                </td>
                <td data-label="最后在线">{a.online ? fmtTime(a.last_seen) : `离线 ${fmtOffline(a)}`}</td>
                <td data-label="趋势">
                  <button className="secondary btn-sm" onClick={() => setSelectedAgent(trendOpen ? '' : a.agent_id)}>
                    {trendOpen ? '收起' : '查看'}
                  </button>
                  {!a.online && (
                    <button
                      className="secondary danger btn-sm"
                      style={{ marginLeft: 6 }}
                      onClick={() => removeAgent(a)}
                      title="删除这台离线主机的登记与历史数据"
                    >删除</button>
                  )}
                </td>
              </tr>
              {/* 趋势图**紧跟该条记录下方**展开（原实现放在列表最底部，移动端要滚很久） */}
              {trendOpen && (
                <tr className="trend-row">
                  <td colSpan="13" data-label="趋势图">
                    <div className="trend-inline">
                      <div className="trend-title">
                        探针历史趋势 · {nameOf(a)}
                        <span className="trend-tip">与上方「最近时段」筛选联动</span>
                      </div>
                      <TrendChart history={history || []} />
                    </div>
                  </td>
                </tr>
              )}
              </React.Fragment>
              )
            })}
            {sortedAgents.length === 0 && (
              <tr><td colSpan="13" className="table-empty">暂无主机接入</td></tr>
            )}
          </tbody>
        </table>
        </div>
        <Pagination
          total={sortedAgents.length}
          page={curPage}
          pageSize={pageSize}
          onPage={setPage}
          onPageSize={changePageSize}
          loading={false}
        />
      </div>

    </div>
  )
}

// OverviewMetric 总览卡片头部的单个指标。
//
// 用 span 而不是 div：它嵌在 h3 同一行的 flex 容器里，块级元素会把行高撑坏。
function OverviewMetric({ value, label, sub, tone }) {
  return (
    <span className={`ov-item ${tone || ''}`}>
      <span className="ov-v">{value}</span>
      <span className="ov-l">{label}{sub ? <i className="metric-note"> {sub}</i> : null}</span>
    </span>
  )
}

// 轻量 SVG 趋势折线图（无外部依赖）
//
// 交互：鼠标移入 / 手指在图上滑动或点按后，显示当前横轴位置**所有曲线**的读数与采样时间。
// 折线图只能看出"大概在涨"，运维实际要的是"上午十点 CPU 到底多少"，
// 因此这里补上竖向指示线与悬浮数值浮层。
// 触摸端刻意不做"抬手即隐藏"：手指会挡住浮层，抬手就消失根本读不到数，
// 改为保留至下一次点按。
function TrendChart({ history }) {
  const W = 900, H = 240, PAD = 30
  // 悬浮选中的采样点下标；null = 未选中
  const [hover, setHover] = useState(null)
  const wrapRef = useRef(null)
  const series = useMemo(() => (history || []).slice(-200), [history])

  if (!history || history.length === 0) {
    return <div style={{ color: '#8b98a5', textAlign: 'center', padding: 30 }}>该时间段暂无历史采样数据</div>
  }

  const n = series.length
  // 单点时 div 会为 0：直接按整段宽度画，避免除零得到 NaN 坐标
  const xOf = (i) => (n <= 1 ? PAD : PAD + (i / (n - 1)) * (W - PAD * 2))

  // 数值 -> 坐标。最大值参与的是**所有曲线**（保持 0-100% 网格语义），
  // 若只按当前曲线取最大值，三条线的相对高低就失去意义了。
  const allVals = []
  for (const p of series) for (const k of ['cpu_usage', 'mem_usage', 'disk_usage']) allVals.push(Number(p[k]) || 0)
  const max = Math.max(100, ...allVals)
  const yOf = (v) => H - PAD - (Math.max(0, v) / max) * (H - PAD * 2)

  const lines = [
    { key: 'cpu_usage', color: '#4da3ff', label: 'CPU' },
    { key: 'mem_usage', color: '#52c41a', label: '内存' },
    { key: 'disk_usage', color: '#fa8c16', label: '磁盘' },
  ]

  // 把浏览器坐标换算成采样点下标。
  // getBoundingClientRect 给的是 CSS 像素，viewBox 是逻辑坐标，必须按实际宽度缩放，
  // 否则窄屏（SVG 被压到 320px）时选中的点会整体偏移。
  const pick = (clientX) => {
    const el = wrapRef.current
    if (!el) return null
    const rect = el.getBoundingClientRect()
    if (rect.width <= 0) return null
    const x = ((clientX - rect.left) / rect.width) * W
    const ratio = (x - PAD) / (W - PAD * 2)
    const i = Math.round(ratio * (n - 1))
    return Math.min(n - 1, Math.max(0, i))
  }

  const onMove = (e) => setHover(pick(e.clientX))
  // 触摸设备没有 hover：按下即定位，抬起后保留一会儿便于读数（手指会挡住浮层）
  const onTouch = (e) => {
    const t = e.touches?.[0] || e.changedTouches?.[0]
    if (t) setHover(pick(t.clientX))
  }

  const cur = hover === null ? null : series[hover]
  // 浮层靠右时贴到指示线左侧，避免被 SVG 右边界裁掉
  const tipLeft = cur ? (xOf(hover) / W) * 100 : 0
  const tipFlip = tipLeft > 62

  return (
    <div className="trend-chart" ref={wrapRef} onMouseMove={onMove} onMouseLeave={() => setHover(null)}
      onTouchStart={onTouch} onTouchMove={onTouch}>
      <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 'auto' }}>
        {/* 网格线 */}
        {[0, 25, 50, 75, 100].map((g) => {
          const y = H - PAD - (g / 100) * (H - PAD * 2)
          return <g key={g}>
            <line x1={PAD} y1={y} x2={W - PAD} y2={y} stroke="#2a3340" strokeDasharray="4 4" />
            <text x={PAD - 6} y={y + 4} fill="#8b98a5" fontSize="10" textAnchor="end">{g}%</text>
          </g>
        })}

        {lines.map(({ key, color }) => {
          const vals = series.map((p) => Number(p[key]) || 0)
          const pts = vals.map((v, i) => `${xOf(i).toFixed(1)},${yOf(v).toFixed(1)}`).join(' ')
          return <polyline key={key} points={pts} fill="none" stroke={color} strokeWidth="2"
            strokeLinejoin="round" strokeLinecap="round" />
        })}

        {/* 选中位置的竖向指示线 + 三个采样点 */}
        {cur && (
          <g>
            <line x1={xOf(hover)} y1={PAD - 8} x2={xOf(hover)} y2={H - PAD} stroke="#8b98a5" strokeDasharray="3 3" />
            {lines.map(({ key, color }) => (
              <circle key={key} cx={xOf(hover)} cy={yOf(Number(cur[key]) || 0)} r="3.5" fill={color}
                stroke="#0f1519" strokeWidth="1.5" />
            ))}
          </g>
        )}
      </svg>

      {/* 数值浮层：一次展示全部曲线，避免为了看内存再移动一次鼠标。
          用百分比定位而非 SVG 内 text —— 字号不随画布缩放，窄屏也读得清。 */}
      {cur && (
        <div className={`trend-tipbox ${tipFlip ? 'flip' : ''}`} style={{ left: `${tipLeft}%` }}>
          <div className="tip-time">{fmtTimeShort(cur.ts || cur.timestamp || 0)}</div>
          {lines.map(({ key, color, label }) => (
            <div className="tip-row" key={key}>
              <i style={{ background: color }} />{label}
              <b>{Math.round(Number(cur[key]) || 0)}%</b>
            </div>
          ))}
        </div>
      )}
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

// 字节数 -> xx.xxG（内存/磁盘 已用/总量展示）
function fmtGBytes(b) {
  if (!b || b <= 0) return '0.00G'
  return (b / 1024 / 1024 / 1024).toFixed(2) + 'G'
}

// 字节数 -> xx.xxGB（网络累计上行/下行展示，统一 GB）
function fmtGB(b) {
  if (!b || b <= 0) return '0.00GB'
  return (b / 1024 / 1024 / 1024).toFixed(2) + 'GB'
}

// 字节/秒 -> 可读速率（KB/s / MB/s）
function fmtSpeed(bps) {
  if (!bps || bps <= 0) return '-'
  if (bps >= 1024 * 1024) return (bps / 1024 / 1024).toFixed(2) + 'MB/s'
  if (bps >= 1024) return (bps / 1024).toFixed(1) + 'KB/s'
  return bps + 'B/s'
}

function fmtTime(ts) {
  if (!ts) return '-'
  const d = new Date(ts * 1000)
  return d.toLocaleString()
}

// fmtTimeShort 折线图浮层用的短时间（同一天只显示 时:分，跨天带日期）
function fmtTimeShort(ts) {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function fmtOffline(a) {
  const diff = (Date.now() / 1000) - (a.last_offline_at || a.last_seen || 0)
  const s = Math.max(0, Math.floor(diff))
  if (s < 60) return `${s}秒`
  if (s < 3600) return `${Math.floor(s / 60)}分`
  if (s < 86400) return `${Math.floor(s / 3600)}时`
  return `${Math.floor(s / 86400)}天`
}

// LatencyCell 主机延迟概览：只显示"最快目标"与最差目标，避免表格里塞一长串。
//
// 为什么显示范围而不是平均值：平均延迟会掩盖抖动（1ms 与 500ms 各半 -> 平均 250ms），
// 而运维真正关心的是"抖动大不大"。
function LatencyCell({ agent }) {
  const entries = Object.entries(agent.latency || {})
  if (entries.length === 0) {
    return <span style={{ color: '#6b7280', fontSize: 11 }}>—</span>
  }
  const vals = entries.map(([, v]) => v)
  const best = Math.min(...vals)
  const worst = Math.max(...vals)
  // 已配置但本机没探到的目标 = 本轮超时，单独标出来（不能当成 0）
  const total = (agent.latency_targets || []).length
  const lost = Math.max(0, total - entries.length)
  return (
    <div style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
      <div>{round1(best)} ~ {round1(worst)} ms</div>
      <div style={{ color: '#8b98a5', fontSize: 10 }}>
        {entries.length} 个目标{lost > 0 ? ` · ${lost} 个超时` : ''}
      </div>
    </div>
  )
}

// AgentVersionCell 展示 Agent 版本，并标出与当前服务端版本不一致的情况。
//
// 空版本（旧版 Agent 未上报）单独显示为「未知」—— 它一定不是当前版本，
// 但不能显示成"不匹配某个具体版本"，否则运维会以为有明确的版本号。
function AgentVersionCell({ agent }) {
  const v = (agent.agent_version || '').trim()
  if (!v) {
    return <span style={{ color: '#8b98a5', fontSize: 11 }} title="该 Agent 未上报版本号（旧版本）">未知</span>
  }
  return <span style={{ fontSize: 11 }}>{v}</span>
}

function round1(v) {
  return Math.round(v * 10) / 10
}

// 公网 IP 与地理位置展示单元格。
function PublicIPCell({ agent }) {
  const ips = [agent.public_ip, agent.public_ipv6].filter(Boolean)
  const geo = agent.geo
  return (
    <div>
      {ips.length === 0 ? (
        <span style={{ color: '#8b98a5', fontSize: 12 }}>未获取到公网 IP</span>
      ) : (
        ips.map((ip, i) => (
          <div key={i} style={{ fontSize: 12 }} title={ip.includes(':') ? 'IPv6' : 'IPv4'}>
            <span style={{ color: '#8b98a5' }}>{ip.includes(':') ? 'v6' : 'v4'}</span>{' '}
            {ip}
          </div>
        ))
      )}
      {geo && geo.country_code && (
        <div style={{ fontSize: 12, color: '#4da3ff', marginTop: 2 }}>
          <Flag code={geo.country_code} style={{ marginRight: 4 }} />
          {[geo.country, geo.region, geo.city].filter(Boolean).join(' · ')}
        </div>
      )}
    </div>
  )
}

// 备注编辑器：点击主机的备注图标可编辑。
function RemarkEditor({ agent, onSaved }) {
  const [editing, setEditing] = React.useState(false)
  const [value, setValue] = React.useState(agent.remark || '')
  const [saving, setSaving] = React.useState(false)

  const save = async () => {
    setSaving(true)
    try {
      await Api.setRemark(agent.agent_id, value.trim())
      onSaved && onSaved()
      setEditing(false)
    } catch (e) {
      alert('保存备注失败：' + (e.response?.data?.msg || e.message))
    } finally {
      setSaving(false)
    }
  }

  if (!editing) {
    return (
      <div style={{ marginTop: 2 }}>
        <button
          className="secondary"
          style={{ fontSize: 11, padding: '1px 6px' }}
          onClick={() => { setValue(agent.remark || ''); setEditing(true) }}
          title="编辑备注"
        >✎ 备注</button>
      </div>
    )
  }
  return (
    <div style={{ marginTop: 4, display: 'flex', gap: 4, alignItems: 'center' }}>
      <input
        value={value}
        onChange={(e) => setValue(e.target.value)}
        placeholder="输入备注…"
        autoFocus
        style={{ fontSize: 12, padding: '2px 6px', width: 120 }}
        onKeyDown={(e) => { if (e.key === 'Enter') save(); if (e.key === 'Escape') setEditing(false) }}
      />
      <button className="primary" style={{ fontSize: 11, padding: '2px 8px' }} onClick={save} disabled={saving}>
        {saving ? '保存中…' : '保存'}
      </button>
      <button className="secondary" style={{ fontSize: 11, padding: '2px 8px' }} onClick={() => setEditing(false)}>取消</button>
    </div>
  )
}
