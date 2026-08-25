import React, { useState, useEffect } from 'react'
import { Api } from '../api/api.js'

// 端口映射管理界面：配置/查看/管理内网主机端口映射规则
export default function PortMap({ agents }) {
  const [rules, setRules] = useState([
    // MVP 演示数据；实际由服务端持久化并通过 API 返回
  ])
  const [agent, setAgent] = useState('')
  const [pubPort, setPubPort] = useState('')
  const [localPort, setLocalPort] = useState('')
  const [sourceIp, setSourceIp] = useState('')
  const [notice, setNotice] = useState('')

  const addRule = (e) => {
    e.preventDefault()
    if (!agent || !pubPort || !localPort) {
      setNotice('请填写目标主机、公网端口与本地端口')
      return
    }
    setRules([...rules, {
      id: 'rule-' + Date.now(),
      agent_id: agent,
      public_port: Number(pubPort),
      local_port: Number(localPort),
      source_ip: sourceIp,
      enabled: true,
      created_at: Math.floor(Date.now() / 1000),
    }])
    setNotice('映射规则已添加（演示模式，未持久化）')
  }

  const toggleRule = (id) => {
    setRules(rules.map((r) => r.id === id ? { ...r, enabled: !r.enabled } : r))
  }

  const removeRule = (id) => {
    setRules(rules.filter((r) => r.id !== id))
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
                {agents.filter((a) => a.online).map((a) => (
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
        {notice && <div className="notice ok" style={{ marginTop: 12 }}>{notice}</div>}
      </div>

      <div className="card">
        <h3>映射规则列表</h3>
        <table>
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
              <tr key={r.id}>
                <td>{r.id}</td>
                <td>{r.agent_id}</td>
                <td>{r.public_port}</td>
                <td>{r.local_port}</td>
                <td>{r.source_ip || '不限'}</td>
                <td>
                  <span className={`badge ${r.enabled ? 'online' : 'offline'}`}>
                    {r.enabled ? '已启用' : '已禁用'}
                  </span>
                </td>
                <td>
                  <button className="secondary" onClick={() => toggleRule(r.id)} style={{ marginRight: 8 }}>
                    {r.enabled ? '禁用' : '启用'}
                  </button>
                  <button className="secondary" onClick={() => removeRule(r.id)}>删除</button>
                </td>
              </tr>
            ))}
            {rules.length === 0 && (
              <tr><td colSpan="7" style={{ textAlign: 'center', color: '#8b98a5' }}>暂无映射规则</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
