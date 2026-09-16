import React, { useEffect, useMemo, useState } from 'react'
import { Api } from '../api/api.js'

// 展示配置：控制**未登录访客**在公开状态页能看到的条目。
//
// 交互契约：
//   - 总开关（public_enabled）关闭时，其它开关全部置灰 —— 避免"配置了半天不生效"。
//   - 「是否展示主机」关闭时，主机白名单与逐字段开关置灰（字段仍可预配，便于先配后用）。
//   - 字段开关分为「常规」与「敏感」两类，敏感项单独提示，防止无意间把内网指纹公开。
export default function SiteConfig() {
  const [cfg, setCfg] = useState(null)
  const [fields, setFields] = useState([])
  const [hosts, setHosts] = useState([])
  const [notice, setNotice] = useState('')
  const [ok, setOk] = useState(false)
  const [saving, setSaving] = useState(false)
  const [dirty, setDirty] = useState(false)

  const load = async () => {
    const d = await Api.siteConfig()
    setCfg(normalizeConfig(d.config))
    setFields(Array.isArray(d.fields) ? d.fields : [])
    setDirty(false)
  }

  useEffect(() => {
    load().catch((e) => setNotice('读取展示配置失败：' + (e.response?.data?.msg || e.message)))
    Api.siteHosts()
      .then((d) => setHosts(Array.isArray(d.items) ? d.items : []))
      .catch(() => setHosts([]))
  }, [])

  const groups = useMemo(() => {
    const m = new Map()
    for (const f of fields) {
      if (!m.has(f.group)) m.set(f.group, [])
      m.get(f.group).push(f)
    }
    return [...m.entries()]
  }, [fields])

  const visibleCount = useMemo(
    () => (cfg ? Object.values(cfg.fields || {}).filter(Boolean).length : 0),
    [cfg]
  )

  if (!cfg) {
    return (
      <div>
        <div className="page-head"><div><h2>展示配置</h2></div></div>
        <div className="card">{notice || '正在加载…'}</div>
      </div>
    )
  }

  const patch = (p) => { setCfg({ ...cfg, ...p }); setDirty(true); setNotice('') }
  const toggleField = (key) => {
    setCfg({ ...cfg, fields: { ...cfg.fields, [key]: !cfg.fields[key] } })
    setDirty(true)
  }
  const toggleHost = (agentId) => {
    const has = cfg.host_ids.includes(agentId)
    patch({ host_ids: has ? cfg.host_ids.filter((x) => x !== agentId) : [...cfg.host_ids, agentId] })
  }

  const save = async () => {
    setSaving(true)
    setNotice('')
    try {
      const res = await Api.saveSiteConfig(cfg)
      if (res && res.ok) {
        setOk(true)
        setDirty(false)
        setNotice(res.msg || '已保存')
        await load()
      } else {
        setOk(false)
        setNotice((res && res.msg) || '保存失败')
      }
    } catch (e) {
      setOk(false)
      setNotice('保存失败：' + (e.response?.data?.msg || e.message))
    } finally {
      setSaving(false)
    }
  }

  const reset = async () => {
    if (!window.confirm('恢复默认将关闭公开状态页（总开关关闭），逐字段配置回到初始值。确认继续？')) return
    setSaving(true)
    try {
      const res = await Api.resetSiteConfig()
      setOk(!!res?.ok)
      setNotice(res?.msg || '已恢复默认')
      await load()
    } catch (e) {
      setOk(false)
      setNotice('恢复默认失败：' + (e.response?.data?.msg || e.message))
    } finally {
      setSaving(false)
    }
  }

  const fieldsDisabled = !cfg.public_enabled || !cfg.hosts_enabled

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>展示配置</h2>
          <div className="sub">
            控制未登录访客在公开状态页能看到的信息 · 当前公开字段 {visibleCount} 项
          </div>
        </div>
        <div className="filter-actions">
          <a className="public-btn ghost" href="/public" target="_blank" rel="noreferrer">
            {cfg.public_enabled ? '预览公开页 ↗' : '公开页未启用'}
          </a>
          <button className="secondary" onClick={reset} disabled={saving}>恢复默认</button>
          <button onClick={save} disabled={saving || !dirty}>{saving ? '保存中…' : dirty ? '保存配置' : '已保存'}</button>
        </div>
      </div>

      {notice && <div className={`notice ${ok ? 'ok' : 'err'}`}>{notice}</div>}

      {/* 全局开关 */}
      <div className="card">
        <h3>全局开关</h3>
        <div className="switch-list">
          <Switch
            checked={cfg.public_enabled}
            onChange={(v) => patch({ public_enabled: v })}
            title="启用公开状态页（总开关）"
            desc="关闭时公开页面对外不存在（接口返回 404）。默认关闭；开启后请到下方逐项确认要公开的内容。"
            danger={cfg.public_enabled}
          />
          <Switch
            checked={cfg.hosts_enabled}
            onChange={(v) => patch({ hosts_enabled: v })}
            disabled={!cfg.public_enabled}
            title="主页展示主机"
            desc="关闭后公开页只展示站点级汇总指标（节点总数/在线数/平均负载），不展示任何主机条目。"
          />
        </div>
        <div className="form-grid" style={{ marginTop: 14 }}>
          <div className="form-row">
            <label>公开页标题</label>
            <input
              value={cfg.title || ''}
              onChange={(e) => patch({ title: e.target.value })}
              placeholder="MoCenter 节点状态"
              maxLength={60}
            />
          </div>
          <div className="form-row">
            <label>公开页说明</label>
            <input
              value={cfg.description || ''}
              onChange={(e) => patch({ description: e.target.value })}
              placeholder="一句话说明本页用途"
              maxLength={200}
            />
          </div>
        </div>
      </div>

      {/* 主机范围 */}
      <div className="card">
        <h3>主机范围</h3>
        <div className="switch-list">
          <Switch
            checked={cfg.catalog_enabled}
            onChange={(v) => patch({ catalog_enabled: v })}
            disabled={!cfg.public_enabled || !cfg.hosts_enabled}
            title="展示全部主机"
            desc="开启 = 公开页列出所有接入主机；关闭 = 只公开下方勾选的主机（白名单）。"
          />
        </div>

        {cfg.public_enabled && cfg.hosts_enabled && !cfg.catalog_enabled && (
          <>
            <div className="sub" style={{ color: 'var(--muted)', fontSize: 13, margin: '12px 0 8px' }}>
              已选 {cfg.host_ids.length} 台（{cfg.host_ids.length === 0 ? '当前未选择任何主机，公开页将不展示主机条目' : '仅这些主机对访客可见'}）
            </div>
            <div className="host-picker">
              {hosts.length === 0 && <div className="cell-sub">暂无已接入主机</div>}
              {hosts.map((h) => (
                <label className="checkbox-item" key={h.agent_id} title={h.agent_id}>
                  <input
                    type="checkbox"
                    checked={cfg.host_ids.includes(h.agent_id)}
                    onChange={() => toggleHost(h.agent_id)}
                  />
                  <span className={`dot ${h.online ? 'online' : 'offline'}`} />
                  {h.remark || h.hostname || h.agent_id}
                </label>
              ))}
            </div>
          </>
        )}
        {cfg.catalog_enabled && cfg.public_enabled && cfg.hosts_enabled && (
          <div className="cell-sub" style={{ marginTop: 8 }}>
            当前所有接入主机的非敏感信息都会对访客公开；如需只公开部分主机，请关闭上方开关后勾选。
          </div>
        )}
      </div>

      {/* 字段开关 */}
      <div className="card">
        <h3>展示信息项</h3>
        <div className="desc">
          只有勾选的项才会出现在公开接口的响应里（未勾选 = 数据根本不下发，而非前端隐藏）。
          <br />
          带 <span className="badge cat-op">敏感</span> 的项可能暴露内网拓扑或漏洞指纹，公开前请确认风险。
        </div>
        {fieldsDisabled && (
          <div className="notice err" style={{ marginBottom: 12 }}>
            已关闭「启用公开状态页」或「主页展示主机」，字段配置暂不生效。
          </div>
        )}
        {groups.map(([group, items]) => (
          <div key={group} style={{ marginBottom: 18 }}>
            <div className="field-group-title">{group}</div>
            <div className="field-grid">
              {items.map((f) => (
                <label className={`field-item ${f.sensitive ? 'sensitive' : ''}`} key={f.key} title={f.description}>
                  <input
                    type="checkbox"
                    checked={!!cfg.fields[f.key]}
                    onChange={() => toggleField(f.key)}
                    disabled={fieldsDisabled}
                  />
                  <span className="field-label">
                    {f.label}
                    {f.sensitive && <span className="badge cat-op" style={{ marginLeft: 6 }}>敏感</span>}
                  </span>
                  <span className="field-desc">{f.description}</span>
                </label>
              ))}
            </div>
          </div>
        ))}
        <div className="filter-actions" style={{ marginTop: 4 }}>
          <button
            className="secondary btn-sm"
            onClick={() => { setCfg({ ...cfg, fields: defaultFields(fields) }); setDirty(true) }}
            disabled={fieldsDisabled}
          >按推荐默认（仅非敏感项）</button>
          <button
            className="secondary btn-sm"
            onClick={() => { setCfg({ ...cfg, fields: allFields(fields, false) }); setDirty(true) }}
            disabled={fieldsDisabled}
          >全不选</button>
        </div>
      </div>

      <div className="card">
        <h3>安全说明</h3>
        <ul className="public-tips">
          <li>公开接口从来不返回主机标识：节点对外只有派生编号（<code>n-xxxxxxxx</code>），无法反推 <code>agent_id</code>、硬件 UUID 与内网地址。</li>
          <li>公网 IP 即使开启也会做部分掩码（<code>203.0.*.*</code>），精确经纬度永不公开。</li>
          <li>公开页的实时推送与查询走独立限流，且总开关关闭时端点直接 404。</li>
          <li>本页的每一次保存都会写入操作审计（分类 <code>op</code>），可在「操作审计」中追溯。</li>
        </ul>
      </div>
    </div>
  )
}

// 规范化后端返回的配置，保证前端不会因为字段缺失而崩。
function normalizeConfig(c) {
  const src = c || {}
  return {
    public_enabled: !!src.public_enabled,
    hosts_enabled: src.hosts_enabled !== false,
    catalog_enabled: src.catalog_enabled !== false,
    host_ids: Array.isArray(src.host_ids) ? src.host_ids : [],
    fields: src.fields && typeof src.fields === 'object' ? { ...src.fields } : {},
    title: src.title || '',
    description: src.description || '',
  }
}

function defaultFields(fields) {
  const out = {}
  for (const f of fields) out[f.key] = !!f.default
  return out
}

function allFields(fields, value) {
  const out = {}
  for (const f of fields) out[f.key] = value
  return out
}

function Switch({ checked, onChange, title, desc, disabled, danger }) {
  return (
    <div className={`switch-row ${disabled ? 'disabled' : ''}`}>
      <div className="switch-text">
        <div className="switch-title">
          {title}
          {danger && <span className="badge cat-op" style={{ marginLeft: 8 }}>对外可见</span>}
        </div>
        {desc && <div className="switch-desc">{desc}</div>}
      </div>
      <button
        type="button"
        className={`switch ${checked ? 'on' : ''}`}
        onClick={() => !disabled && onChange(!checked)}
        disabled={disabled}
        aria-pressed={checked}
        aria-label={title}
      >
        <span className="knob" />
      </button>
    </div>
  )
}
