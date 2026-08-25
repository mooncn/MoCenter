import React, { useState } from 'react'
import { Api } from '../api/api.js'

// 运维操作页面：批量命令、远程升级、文件传输
export default function Ops({ agents }) {
  const online = agents.filter((a) => a.online)

  // 批量命令
  const [selected, setSelected] = useState([])
  const [batchCmd, setBatchCmd] = useState('')
  const [batchShell, setBatchShell] = useState('auto')
  const [batchResult, setBatchResult] = useState(null)
  const [batchNotice, setBatchNotice] = useState('')

  // 远程升级
  const [upAgent, setUpAgent] = useState('')
  const [upVersion, setUpVersion] = useState('')
  const [upUrl, setUpUrl] = useState('')
  const [upMd5, setUpMd5] = useState('')
  const [upNotice, setUpNotice] = useState('')

  const toggleSelect = (id) => {
    setSelected(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id])
  }

  const runBatch = async (e) => {
    e.preventDefault()
    setBatchResult(null)
    setBatchNotice('')
    if (selected.length === 0) {
      setBatchNotice('请至少选择一台目标主机')
      return
    }
    if (!batchCmd.trim()) {
      setBatchNotice('请输入要执行的命令')
      return
    }
    try {
      const res = await Api.batchCommand({
        agent_ids: selected,
        command: batchCmd,
        shell_type: batchShell === 'auto' ? '' : batchShell,
        timeout: 30,
      })
      setBatchResult(res.results || [])
      setBatchNotice(res.ok ? `已下发到 ${selected.length} 台主机` : '下发失败')
    } catch (ex) {
      setBatchNotice('下发失败：' + (ex.response?.data?.msg || ex.message))
    }
  }

  const runUpgrade = async (e) => {
    e.preventDefault()
    setUpNotice('')
    if (!upAgent) {
      setUpNotice('请选择目标主机')
      return
    }
    try {
      const res = await Api.upgrade({
        agent_id: upAgent,
        version: upVersion,
        url: upUrl,
        md5: upMd5,
      })
      setUpNotice(res.ok ? '升级指令已下发，等待 Agent 执行...' : res.msg)
    } catch (ex) {
      setUpNotice('下发失败：' + (ex.response?.data?.msg || ex.message))
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>运维操作</h2>
          <div className="sub">批量命令 · 远程升级 · 文件传输</div>
        </div>
      </div>

      {/* 批量命令 */}
      <div className="card">
        <h3>批量命令</h3>
        <div className="form-row">
          <label>选择目标主机（多选）</label>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
            {online.map((a) => (
              <label key={a.agent_id} className="checkbox-item">
                <input type="checkbox" checked={selected.includes(a.agent_id)} onChange={() => toggleSelect(a.agent_id)} />
                {a.hostname} ({a.agent_id})
              </label>
            ))}
            {online.length === 0 && <span style={{ color: '#8b98a5' }}>暂无在线主机</span>}
          </div>
        </div>
        <div className="form-grid">
          <div className="form-row">
            <label>Shell 环境</label>
            <select value={batchShell} onChange={(e) => setBatchShell(e.target.value)}>
              <option value="auto">自动推断</option>
              <option value="bash">bash</option>
              <option value="sh">sh</option>
              <option value="cmd">cmd</option>
              <option value="powershell">powershell</option>
            </select>
          </div>
        </div>
        <form onSubmit={runBatch}>
          <div className="form-row">
            <label>命令内容</label>
            <textarea
              rows={3}
              value={batchCmd}
              onChange={(e) => setBatchCmd(e.target.value)}
              placeholder="输入需批量执行的命令，如：uptime"
            />
          </div>
          <button type="submit">批量执行</button>
        </form>
        {batchNotice && <div className={`notice ${batchNotice.includes('失败') ? 'err' : 'ok'}`} style={{ marginTop: 12 }}>{batchNotice}</div>}
        {batchResult && (
          <div style={{ marginTop: 16 }}>
            <h4>下发结果</h4>
            <table>
              <thead>
                <tr><th>目标主机</th><th>下发状态</th><th>说明</th></tr>
              </thead>
              <tbody>
                {batchResult.map((r) => (
                  <tr key={r.agent_id}>
                    <td>{r.agent_id}</td>
                    <td><span className={`badge ${r.ok ? 'online' : 'offline'}`}>{r.ok ? '已下发' : '失败'}</span></td>
                    <td>{r.msg || '-'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="sub" style={{ marginTop: 8 }}>执行结果将实时推送到「远程终端」页面对应主机，也可在「操作审计」查看。</div>
          </div>
        )}
      </div>

      {/* 远程升级 */}
      <div className="card" style={{ marginTop: 20 }}>
        <h3>远程升级 Agent</h3>
        <form onSubmit={runUpgrade}>
          <div className="form-grid">
            <div className="form-row">
              <label>目标主机</label>
              <select value={upAgent} onChange={(e) => setUpAgent(e.target.value)}>
                <option value="">选择主机...</option>
                {online.map((a) => (
                  <option key={a.agent_id} value={a.agent_id}>{a.hostname} ({a.agent_id})</option>
                ))}
              </select>
            </div>
            <div className="form-row">
              <label>目标版本</label>
              <input value={upVersion} onChange={(e) => setUpVersion(e.target.value)} placeholder="如 1.1.0" />
            </div>
            <div className="form-row" style={{ gridColumn: '1 / -1' }}>
              <label>升级包下载地址</label>
              <input value={upUrl} onChange={(e) => setUpUrl(e.target.value)} placeholder="https://example.com/mocenter-agent" />
            </div>
            <div className="form-row">
              <label>升级包 MD5（可选）</label>
              <input value={upMd5} onChange={(e) => setUpMd5(e.target.value)} placeholder="校验升级包完整性" />
            </div>
          </div>
          <button type="submit">下发升级</button>
        </form>
        {upNotice && <div className={`notice ${upNotice.includes('失败') ? 'err' : 'ok'}`} style={{ marginTop: 12 }}>{upNotice}</div>}
        <div className="sub" style={{ marginTop: 12 }}>
          升级流程：Agent 下载升级包 → 校验 MD5 → 备份旧版本 → 替换二进制 → 自动重启（失败自动回滚）。
        </div>
      </div>
    </div>
  )
}
