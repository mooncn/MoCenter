import React, { useEffect, useMemo, useState } from 'react'
import { Api } from '../api/api.js'
import { hostLabel } from '../utils/hostName.js'

// 延迟监测页（ICMP / TCP / HTTP + 历史曲线）。
//
// 对应 Issue 需求「P0：延迟监测（ICMP/TCP/HTTP + 历史）」的展示端。
//
// 三个设计要点：
//
//  1. **超时点画成断点，不画成 0**。接口返回的每个点带 ok 标记，
//     失败点（ok=false）不参与折线连接 —— 若把失败当 0 连起来，
//     "完全不通"的机器会显示成"延迟最低"，这是最危险的一类误报。
//  2. **探测目标按主机下发**，不走全局配置：不同机器关心的目标不同
//     （内网机器盯网关，公网机器盯 1.1.1.1 / 8.8.8.8）。
//  3. 每台主机的可用率（丢包率）单独统计：只看平均延迟会漏掉"一半时间超时、
//     一半时间很快"这种典型故障。
export default function Latency({ agents = [] }) {
  const [agentId, setAgentId] = useState('')
  const [range, setRange] = useState('24h')
  const [targetFilter, setTargetFilter] = useState('')
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(false)
  const [msg, setMsg] = useState(null)
  const [editTargets, setEditTargets] = useState('')
  const [saving, setSaving] = useState(false)

  // 默认选中第一台在线主机（离线机器没有延迟数据）
  useEffect(() => {
    if (agentId) return
    const online = agents.find((a) => a.online) || agents[0]
    if (online) setAgentId(online.agent_id)
  }, [agents, agentId])

  const load = () => {
    if (!agentId) return
    setLoading(true)
    Api.latency(agentId, range, targetFilter)
      .then((d) => {
        setData(d)
        // 目标编辑器回填当前生效配置（未编辑时不留空）
        if (d && Array.isArray(d.targets)) setEditTargets(d.targets.join('\n'))
      })
      .catch((e) => setMsg({ type: 'err', text: '加载延迟数据失败: ' + (e.response?.data?.msg || e.message) }))
      .finally(() => setLoading(false))
  }

  useEffect(load, [agentId, range, targetFilter])

  const current = agents.find((a) => a.agent_id === agentId)
  const series = data?.series || []

  // 每个目标的统计：平均/最小/最大/可用率
  const stats = useMemo(() => series.map((s) => {
    const pts = s.points || []
    const okPts = pts.filter((p) => p.ok)
    const lost = pts.length - okPts.length
    const avg = okPts.length ? okPts.reduce((a, p) => a + p.ms, 0) / okPts.length : 0
    const min = okPts.length ? Math.min(...okPts.map((p) => p.ms)) : 0
    const max = okPts.length ? Math.max(...okPts.map((p) => p.ms)) : 0
    return {
      target: s.target, kind: s.kind,
      avg: round1(avg), min: round1(min), max: round1(max),
      loss: pts.length ? round1((lost / pts.length) * 100) : 0,
      total: pts.length, okCount: okPts.length,
    }
  }), [series])

  const saveTargets = async () => {
    if (!agentId) return
    const list = editTargets.split('\n').map((s) => s.trim()).filter(Boolean)
    if (list.length > 8) {
      setMsg({ type: 'err', text: '延迟目标最多 8 个' })
      return
    }
    setSaving(true)
    try {
      const d = await Api.saveLatencyTargets(agentId, list)
      setMsg({ type: d.ok ? 'ok' : 'err', text: d.msg || (d.ok ? '已保存' : '保存失败') })
      load()
    } catch (e) {
      setMsg({ type: 'err', text: '保存失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setSaving(false)
    }
  }

  if (agents.length === 0) {
    return (
      <div className="card">
        <h3>延迟监测</h3>
        <div className="empty">暂无主机。请先部署 Agent 后回到本页配置延迟探测目标。</div>
      </div>
    )
  }

  return (
    <div>
      <div className="card">
        <div className="lat-head">
          <h3>延迟监测</h3>
          <div className="lat-controls">
            <select value={agentId} onChange={(e) => setAgentId(e.target.value)}>
              {agents.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>
                  {hostLabel(a)} {a.online ? '（在线）' : '（离线）'}
                </option>
              ))}
            </select>
            <div className="seg">
              {['1h', '6h', '24h', '7d'].map((r) => (
                <button key={r} className={range === r ? 'on' : ''} onClick={() => setRange(r)}>{r}</button>
              ))}
            </div>
            <button className="btn ghost" onClick={load} disabled={loading}>{loading ? '加载中…' : '刷新'}</button>
          </div>
        </div>

        {msg && <div className={`lat-msg ${msg.type}`}>{msg.text}</div>}

        {current && !current.online && (
          <div className="lat-msg warn">
            该主机当前离线，下方为历史数据。延迟探测需要 Agent 在线才能更新。
          </div>
        )}

        {series.length === 0 && !loading && (
          <div className="empty">
            暂无延迟数据。请在下方「探测目标」中填写目标（每行一个），保存后 Agent 会在下一个采集周期（默认 10 秒）开始探测。
          </div>
        )}

        {stats.length > 0 && (
          <div className="lat-table-wrap">
            <table className="table lat-table">
              <thead>
                <tr>
                  <th>目标</th>
                  <th>方式</th>
                  <th>平均延迟</th>
                  <th>最小</th>
                  <th>最大</th>
                  <th>丢包率</th>
                  <th>样本</th>
                </tr>
              </thead>
              <tbody>
                {stats.map((s) => (
                  <tr key={s.target}
                    className={targetFilter === s.target ? 'on' : ''}
                    onClick={() => setTargetFilter(targetFilter === s.target ? '' : s.target)}
                    title="点击只看该目标的历史曲线">
                    <td className="mono">{s.target}</td>
                    <td><span className={`lat-kind ${s.kind}`}>{s.kind.toUpperCase()}</span></td>
                    <td>{s.okCount ? `${s.avg} ms` : '—'}</td>
                    <td>{s.okCount ? `${s.min} ms` : '—'}</td>
                    <td>{s.okCount ? `${s.max} ms` : '—'}</td>
                    <td className={s.loss > 0 ? 'lat-loss' : ''}>{s.loss}%</td>
                    <td className="cell-sub">{s.okCount}/{s.total}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="public-card-title">
          <h3>延迟曲线</h3>
          <span className="cell-sub">
            {targetFilter ? `目标：${targetFilter}（点击上方表格可切换）` : '全部目标'}
          </span>
        </div>
        {series.length === 0
          ? <div className="empty">暂无数据</div>
          : <LatencyChart series={series} range={range} />}
        <div className="lat-legend-hint">
          曲线中的空白段表示该时刻**探测超时或失败**（不是 0 延迟）。失败点被刻意断开，
          否则超时的目标会看起来比谁都"快"。
        </div>
      </div>

      <div className="card">
        <h3>探测目标</h3>
        <p className="cell-sub" style={{ marginBottom: 10 }}>
          每行一个，支持三种方式，最多 8 个：
          <code>icmp:1.1.1.1</code>、<code>tcp:1.1.1.1:443</code>、<code>http:https://example.com</code>。
          省略前缀时按 ICMP 处理。留空表示不探测。
        </p>
        <textarea
          className="lat-textarea"
          rows={5}
          value={editTargets}
          onChange={(e) => setEditTargets(e.target.value)}
          placeholder={'icmp:1.1.1.1\ntcp:1.1.1.1:443\nhttp:https://www.baidu.com'}
        />
        <div style={{ marginTop: 10, display: 'flex', gap: 8 }}>
          <button className="btn" onClick={saveTargets} disabled={saving || !agentId}>
            {saving ? '保存中…' : '保存并生效'}
          </button>
        </div>
      </div>
    </div>
  )
}

// LatencyChart 延迟折线图（轻量 SVG）。
//
// 每条线按"连续区间"分段：ok=false 的点断开，因此空白处一眼就是超时。
// 纵轴自适应峰值（延迟量级差异很大：内网 0.5ms、跨洋 300ms）。
function LatencyChart({ series, range }) {
  const W = 900, H = 240, PAD_L = 48, PAD_R = 16, PAD_T = 20, PAD_B = 26
  const colors = ['#4da3ff', '#52c41a', '#fa8c16', '#f759ab', '#21d4fd', '#b37feb', '#ffc53d', '#ff7875']

  // 合并所有点的时间轴（各目标采样时刻相同，取并集最稳）
  const tsSet = new Set()
  for (const s of series) for (const p of s.points || []) tsSet.add(p.ts)
  const tsList = Array.from(tsSet).sort((a, b) => a - b)
  if (tsList.length === 0) return <div className="empty">暂无数据</div>
  const tsIndex = new Map(tsList.map((t, i) => [t, i]))

  let max = 0
  for (const s of series) {
    for (const p of s.points || []) if (p.ok && p.ms > max) max = p.ms
  }
  max = max > 0 ? max * 1.15 : 10

  const xOf = (i) => PAD_L + (i / Math.max(1, tsList.length - 1)) * (W - PAD_L - PAD_R)
  const yOf = (v) => H - PAD_B - (Math.max(0, v) / max) * (H - PAD_T - PAD_B)

  const segsOf = (s) => {
    const segs = []
    let cur = []
    for (const p of s.points || []) {
      if (!p.ok) {
        if (cur.length) { segs.push(cur); cur = [] }
        continue
      }
      const i = tsIndex.get(p.ts)
      if (i === undefined) continue
      cur.push(`${xOf(i).toFixed(1)},${yOf(p.ms).toFixed(1)}`)
    }
    if (cur.length) segs.push(cur)
    return segs
  }

  const fmtTs = (ts) => new Date(ts * 1000).toLocaleString([], {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
  })
  const gridVals = [0, 0.25, 0.5, 0.75, 1].map((f) => max * f)

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} className="public-trend-svg">
        {gridVals.map((g, i) => (
          <g key={i}>
            <line x1={PAD_L} y1={yOf(g)} x2={W - PAD_R} y2={yOf(g)} stroke="#2a3340" strokeDasharray="4 4" />
            <text x={PAD_L - 6} y={yOf(g) + 4} fill="#8b98a5" fontSize="10" textAnchor="end">
              {round1(g)}ms
            </text>
          </g>
        ))}
        {series.map((s, idx) => {
          const color = colors[idx % colors.length]
          return (
            <g key={s.target}>
              {segsOf(s).map((pts, i) => (
                <polyline key={i} points={pts.join(' ')} fill="none" stroke={color} strokeWidth="2"
                  strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
              ))}
            </g>
          )
        })}
        <text x={PAD_L} y={H - 8} fill="#8b98a5" fontSize="10">{fmtTs(tsList[0])}</text>
        <text x={W - PAD_R} y={H - 8} fill="#8b98a5" fontSize="10" textAnchor="end">
          {fmtTs(tsList[tsList.length - 1])}
        </text>
      </svg>
      <div className="lat-legend">
        {series.map((s, idx) => (
          <span className="lat-legend-item" key={s.target}>
            <i style={{ background: colors[idx % colors.length] }} />
            {s.target}
          </span>
        ))}
        <span className="cell-sub">窗口：最近 {range}</span>
      </div>
    </div>
  )
}

function round1(v) {
  if (!isFinite(v)) return 0
  return Math.round(v * 10) / 10
}
