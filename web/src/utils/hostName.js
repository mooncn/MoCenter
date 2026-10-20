// 主机展示名：备注优先，其次主机名，最后 UUID（agent_id）。
// 所有后台页面的主机选择器、列表与弹窗都走这里，避免各页口径不一致。
export function hostName(host) {
  if (!host) return ''
  return host.remark || host.hostname || host.agent_id || ''
}

// 带身份后缀的展示名：名称与 agent_id 不同时附上 agent_id，便于在多台同名主机间区分。
export function hostLabel(host) {
  const name = hostName(host)
  const id = host && host.agent_id ? host.agent_id : ''
  if (!name) return id || '—'
  return id && id !== name ? `${name}（${id}）` : name
}

// 只有 agent_id 时（端口映射规则、批量结果等）从主机列表反查展示名。
export function hostLabelById(agents, id, fallback = '—') {
  const rec = (agents || []).find((a) => a && a.agent_id === id)
  return rec ? hostLabel(rec) : (id || fallback)
}
