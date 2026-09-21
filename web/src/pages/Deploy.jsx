import React, { useEffect, useState } from 'react'
import { Api } from '../api/api.js'

// 一键部署 Agent 页。
//
// 需求原文：后台输入账号/密码（Linux 直接 SSH 登录）→ 自动安装服务并写入 Agent 配置。
//
// 交互设计考虑：
//
//   - **先探测再安装**：「测试连接」按钮走 probe_only，只做 SSH 连通性与环境探测，
//     不做任何改动。让运维在真正安装前确认"连得上、架构对、有 root"，
//     避免"点了安装等了半分钟才发现密码写错了"。
//   - **逐步结果展示**：后端返回每一步的输出，前端按顺序列出 ——
//     失败时运维能直接看到卡在哪一步、原始报错是什么。
//   - **凭证不落任何地方**：密码/私钥只在本次请求里用，前端不写入 localStorage，
//     页面上也明确写出这一点。
//   - **主机密钥要看得见**：服务端按 known_hosts TOFU 校验目标机，结果里回显指纹；
//     首次信任时显式标黄提示"请核对"，否则这条链就退化成"连上就算对"。
export default function Deploy() {
  const [form, setForm] = useState({
    host: '', port: 22, user: 'root', password: '', private_key: '',
    agent_id: '', server_addr: '', binary: '', latency: '', host_key_fingerprint: '',
  })
  const [authMode, setAuthMode] = useState('password')
  const [packages, setPackages] = useState([])
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  const [msg, setMsg] = useState(null)

  useEffect(() => {
    Api.deployPackages()
      .then((d) => setPackages(Array.isArray(d.packages) ? d.packages : []))
      .catch(() => {})
  }, [])

  const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }))

  const run = async (probeOnly) => {
    setBusy(true)
    setMsg(null)
    setResult(null)
    try {
      const payload = {
        host: form.host.trim(),
        port: Number(form.port) || 22,
        user: form.user.trim(),
        agent_id: form.agent_id.trim(),
        server_addr: form.server_addr.trim(),
        binary: form.binary,
        host_key_fingerprint: form.host_key_fingerprint.trim(),
        probe_only: probeOnly,
        latency_targets: form.latency.split('\n').map((s) => s.trim()).filter(Boolean),
      }
      if (authMode === 'password') payload.password = form.password
      else payload.private_key = form.private_key

      const d = await Api.deployAgent(payload)
      setResult(d)
      if (!d.ok) setMsg({ type: 'err', text: d.msg || '操作未完成' })
      else setMsg({ type: 'ok', text: d.message || '完成' })
    } catch (e) {
      setMsg({ type: 'err', text: e.response?.data?.msg || e.message })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div>
      <div className="card">
        <h3>一键部署 Agent</h3>
        <p className="cell-sub">
          通过 SSH 登录目标 Linux 主机，上传 Agent、写入配置并注册为系统服务。
          需要目标机可 SSH 登录且具备 root 权限（或免密 sudo）。
          凭证仅用于本次操作，不写入任何存储与日志；目标机的主机密钥会记入服务端数据目录的
          {' '}<code>ssh_known_hosts</code>（首次接触记录，之后变更即拒绝）。
        </p>
        {msg && <div className={`lat-msg ${msg.type}`}>{msg.text}</div>}

        <div className="deploy-grid">
          <label>
            主机地址
            <input value={form.host} onChange={set('host')} placeholder="192.168.1.10 或 host.example.com" />
          </label>
          <label>
            SSH 端口
            <input value={form.port} onChange={set('port')} placeholder="22" />
          </label>
          <label>
            登录用户
            <input value={form.user} onChange={set('user')} placeholder="root" />
          </label>
          <label>
            认证方式
            <select value={authMode} onChange={(e) => setAuthMode(e.target.value)}>
              <option value="password">密码</option>
              <option value="key">私钥</option>
            </select>
          </label>
          {authMode === 'password' ? (
            <label className="span2">
              登录密码
              <input type="password" value={form.password} onChange={set('password')} autoComplete="new-password" />
            </label>
          ) : (
            <label className="span2">
              私钥内容（PEM）
              <textarea rows={5} value={form.private_key} onChange={set('private_key')}
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
            </label>
          )}

          <label>
            Agent 名称（留空用主机名）
            <input value={form.agent_id} onChange={set('agent_id')} placeholder="web-01" />
          </label>
          <label>
            服务端地址（留空用服务端配置）
            <input value={form.server_addr} onChange={set('server_addr')} placeholder="1.2.3.4:9000" />
          </label>
          <label className="span2">
            主机密钥指纹（可选，形如 SHA256:xxx…）
            <input value={form.host_key_fingerprint} onChange={set('host_key_fingerprint')}
              placeholder="留空则首次连接自动记录，之后再变更会被拒绝" autoComplete="off" />
            <span className="cell-sub">
              填写后即强制钉扎：目标机的密钥与这里不一致就中止部署，不会自动放行。
              新机器建议先用「测试连接」拿到指纹，核对后再填入。
            </span>
          </label>
          <label className="span2">
            安装包（留空按目标架构自动选择）
            <select value={form.binary} onChange={set('binary')}>
              <option value="">自动选择</option>
              {packages.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}{p.arch ? `（${p.arch}）` : ''}
                </option>
              ))}
            </select>
            {packages.length === 0 && (
              <span className="cell-sub">
                服务端安装包目录为空：请先构建 Agent（scripts/build-linux-all.sh --type=agent）并把
                mocenter-agent-&lt;arch&gt; 放进数据目录的 packages/ 下。
              </span>
            )}
          </label>
          <label className="span2">
            延迟探测目标（可选，每行一个）
            <textarea rows={3} value={form.latency} onChange={set('latency')}
              placeholder={'icmp:1.1.1.1\ntcp:1.1.1.1:443'} />
          </label>
        </div>

        <div className="deploy-actions">
          <button className="btn ghost" onClick={() => run(true)} disabled={busy || !form.host}>
            {busy ? '执行中…' : '测试连接与探测环境'}
          </button>
          <button className="btn" onClick={() => run(false)} disabled={busy || !form.host}>
            {busy ? '执行中…' : '开始部署'}
          </button>
        </div>
      </div>

      {result?.target && (
        <div className="card">
          <h3>目标机信息</h3>
          <div className="public-facts">
            <div className="public-fact"><span className="k">主机名</span><span className="v">{result.target.hostname || '—'}</span></div>
            <div className="public-fact"><span className="k">系统</span><span className="v">{result.target.os_name || '—'}</span></div>
            <div className="public-fact"><span className="k">架构</span><span className="v">{result.target.arch}（{result.target.goarch}）</span></div>
            <div className="public-fact"><span className="k">root</span><span className="v">{result.target.is_root ? '是' : `否（sudo: ${result.target.sudo_ok ? '可用' : '不可用'}）`}</span></div>
            <div className="public-fact"><span className="k">systemd</span><span className="v">{result.target.has_systemd ? '可用' : '不可用（将用进程守护方式）'}</span></div>
          </div>
        </div>
      )}

      {result?.host_key && (
        <div className="card">
          <h3>主机密钥</h3>
          <div className="public-facts">
            <div className="public-fact"><span className="k">指纹</span><span className="v">{result.host_key}</span></div>
            <div className="public-fact"><span className="k">信任来源</span>
              <span className="v">{result.host_key_new_trust ? '本次首次记录' : '与已知记录一致'}</span></div>
          </div>
          {result.host_key_new_trust && (
            <div className="lat-msg warn">
              这是服务端第一次连接该地址，密钥已被自动记录。请到目标机上执行
              {' '}<code>ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub</code> 比对指纹；
              若此处指纹是攻击者提供的，后续部署都会信任它。
            </div>
          )}
        </div>
      )}

      {result?.steps?.length > 0 && (
        <div className="card">
          <h3>执行步骤</h3>
          <ol className="deploy-steps">
            {result.steps.map((s, i) => (
              <li key={i} className={s.ok ? 'ok' : 'err'}>
                <div className="deploy-step-head">
                  <span className="deploy-step-icon">{s.ok ? '✓' : '✕'}</span>
                  <span className="deploy-step-name">{s.name}</span>
                  {s.elapsed && <span className="cell-sub">{s.elapsed}</span>}
                </div>
                {s.output && <pre className="deploy-out">{s.output}</pre>}
                {s.error && <pre className="deploy-out err">{s.error}</pre>}
              </li>
            ))}
          </ol>
        </div>
      )}
    </div>
  )
}
