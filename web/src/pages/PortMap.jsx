import React, { useState, useEffect } from 'react'
import { Api } from '../api/api.js'

// 端口映射管理界面：配置/查看/管理内网主机端口映射规则
// 通过服务端 REST API 持久化管理规则，公网端口由服务端反向代理转发到内网
export default function PortMap({ agents }) {
  const [rules, setRules] = useState([])
  const [agent, setAgent] = useState('')
  const [pubPort, setPubPort] = useState('')
  const [localPort, setLocalPort] = useState('')
  const [sourceIp, setSourceIp] = useState('')
  const [notice, setNotice] = useState('')
  const [loading, setLoading] = useState(true)

  // 加载服务端已持久化的映射规则
  const loadRules = () => {
    Api.portmapRules()
      .then((data) => setRules(Array.isArray(data) ? data : []))
      .catch(() => {})
      .finally(() => setLoading(false))
  }

  useEffect(() => {
    loadRules()
  }, [])

  const addRule = async (e) => {
    e.preventDefault()
    setNotice('')
    if (!agent || !pubPort || !localPort) {
      setNotice('请填写目标主机、公网端口与本地端口')
      return
    }
    const p = Number(pubPort)
    const l = Number(localPort)
    if (p < 1 || p > 65535 || l < 1 || l > 65535) {
      setNotice('端口范围应为 1-65535')
      return
    }
    try {
      await Api.portmapOp({
        action: 'create',
        agent_id: agent,
        public_port: p,
        local_port: l,
        source_ip: sourceIp || '',
      })
      setNotice(`映射规则已创建：公网 :${p} -> ${agent}:${l}`)
      setPubPort('')
      setLocalPort('')
      setSourceIp('')
      loadRules()
    } catch (ex) {
      setNotice('创建失败：' + (ex.response?.data?.msg || ex.message))
    }
  }

  const toggleRule = async (r) => {
    await Api.portmapOp({
      action: r.enabled ? 'disable' : 'enable',
      agent_id: r.agent_id,
      public_port: r.public_port,
      local_port: r.local_port,
    })
    loadRules()
  }

  const removeRule = async (r) => {
    await Api.portmapOp({ action: 'delete', public_port: r.public_port })
    loadRules()
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>端口映射</h2>
          <div className="sub">将内网服务端口映射到公网服务端，实现公网访问内网服务</div>
        </div>
      </div>

      <div className="card">
        <h3>新建映射规则</h3>
        <form onSubmit={addRule}>
          <div className="form-grid">
            <div className="form-row">
              <label>目标主机</label>
              <select value={agent} onChange={(e) => setAgent(e.target.value)}>
                <option value="">选择主机...</option>
                {(agents || []).filter((a) => a.online).map((a) => (
                  <option key={a.agent_id} value={a.agent_id}>{a.hostname}</option>
                ))}
              </select>
            </div>
            <div className="form-row">
              <label>公网端口</label>
              <input type="number" value={pubPort} onChange={(e) => setPubPort(e.target.value)} placeholder="如 8080" />
            </div>
            <div className="form-row">
              <label>内网服务端口</label>
              <input type="number" value={localPort} onChange={(e) => setLocalPort(e.target.value)} placeholder="如 80" />
            </div>
            <div className="form-row">
              <label>源 IP 白名单（可选）</label>
              <input value={sourceIp} onChange={(e) => setSourceIp(e.target.value)} placeholder="留空表示不限制" />
            </div>
          </div>
          <button type="submit">添加规则</button>
        </form>
        {notice && <div className={`notice ${notice.includes('失败') ? 'err' : 'ok'}`} style={{ marginTop: 12 }}>{notice}</div>}
      </div>

      <div className="card">
        <h3>映射规则列表</h3>
        <div className="table-scroll">
        <table className="table-stack">
          <thead>
            <tr>
              <th>规则 ID</th>
              <th>目标主机</th>
              <th>公网端口</th>
              <th>内网端口</th>
              <th>源 IP 白名单</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {rules.map((r) => (
              <tr key={r.public_port || r.id}>
                <td data-label="规则 ID">{r.id || `rule-${r.public_port}`}</td>
                <td data-label="目标主机">{r.agent_id}</td>
                <td data-label="公网端口">{r.public_port}</td>
                <td data-label="内网端口">{r.local_port}</td>
                <td data-label="源 IP 白名单">{r.source_ip_whitelist || r.source_ip || '不限'}</td>
                <td data-label="状态">
                  <span className={`badge ${r.enabled ? 'online' : 'offline'}`}>
                    {r.enabled ? '已启用' : '已禁用'}
                  </span>
                </td>
                <td data-label="操作">
                  <span style={{ display: 'inline-flex', gap: 8 }}>
                    <button className="secondary btn-sm" onClick={() => toggleRule(r)}>
                      {r.enabled ? '禁用' : '启用'}
                    </button>
                    <button className="secondary btn-sm danger" onClick={() => removeRule(r)}>删除</button>
                  </span>
                </td>
              </tr>
            ))}
            {!loading && rules.length === 0 && (
              <tr><td colSpan="7" className="table-empty">暂无映射规则</td></tr>
            )}
          </tbody>
        </table>
        </div>
      </div>
    </div>
  )
}
