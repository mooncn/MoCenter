import React, { useEffect, useState } from 'react'
import { Api } from '../api/api.js'

// maskedKey 只露出首尾各 6 位：既能让运维确认"是不是这一把"，
// 又避免密钥长时间明摆在屏幕上（截图/投屏会泄露）。
function maskedKey(key) {
  if (!key) return ''
  if (key.length <= 16) return '•'.repeat(key.length)
  return `${key.slice(0, 6)}${'•'.repeat(16)}${key.slice(-6)}`
}

// Agent 接入：把"接入一台新主机"需要的信息集中到一页，省掉翻服务器/找文件的步骤。
//
// 以前要配双向认证得手工生成 ca/server/client 三套证书、分别拷到两侧、还要保证
// 服务端证书 SAN 写的是 Agent 连的那个地址 —— 少一步就 TLS 校验失败。
// 现在服务端自己就是 CA：运维只需要把下面这条命令粘到新主机上执行。
export default function AgentEnroll() {
  const [info, setInfo] = useState(null)
  const [err, setErr] = useState('')
  const [copied, setCopied] = useState('')
  const [showKey, setShowKey] = useState(false)

  useEffect(() => {
    let alive = true
    Api.serverInfo()
      .then((d) => { if (alive) setInfo(d) })
      .catch((e) => { if (alive) setErr(e?.response?.data?.msg || e.message || '获取失败') })
    return () => { alive = false }
  }, [])

  const copy = async (text, tag) => {
    try {
      await navigator.clipboard.writeText(text)
    } catch {
      // 非 HTTPS / 无剪贴板权限时的兜底：选中文本让用户手动复制
      window.prompt('复制下面的内容（Ctrl+C）', text)
    }
    setCopied(tag)
    setTimeout(() => setCopied(''), 1600)
  }

  if (err) {
    return <div className="page"><div className="card"><p className="muted">获取服务端信息失败：{err}</p></div></div>
  }
  if (!info) {
    return <div className="page"><div className="card"><p className="muted">加载中…</p></div></div>
  }

  const host = (info.listen || '').replace(/^:/, '')
  const server = host && !host.startsWith('0.0.0.0') ? host : '<总控地址>:9000'
  const keyArg = info.agent_key ? info.agent_key : '<注册密钥>'
  const linuxCmd = `./mocenter-agent -server ${server} -agentid <主机名> -agent-key ${keyArg} -daemon`
  const winCmd = `mocenter-deploy.exe install -role agent -server ${server} -agent-key ${keyArg}`
  const fpCmd = `./mocenter-agent -server ${server} -agentid <主机名> -server-fingerprint ${info.fingerprint || '<指纹>'}`

  return (
    <div className="page">
      <h2 className="page-title">Agent 接入</h2>
      <p className="muted" style={{ marginTop: -6 }}>
        接入一台新主机只需要下面这一条命令 —— 加密与双向认证会自动完成，无需在本机准备任何证书文件。
      </p>

      <div className="card">
        <h3>① 部署身份 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（用于确认"连的是哪套总控"）</span></h3>
        <table className="kv">
          <tbody>
            <tr><th>身份名</th><td>{info.identity || '—'}</td></tr>
            <tr>
              <th>身份指纹</th>
              <td>
                <code>{info.fingerprint_display || '—'}</code>
                {info.fingerprint && (
                  <button className="btn-mini" style={{ marginLeft: 8 }}
                    onClick={() => copy(info.fingerprint, 'fp')}>
                    {copied === 'fp' ? '已复制' : '复制'}
                  </button>
                )}
              </td>
            </tr>
            <tr><th>Agent 通道</th><td>{info.listen || '—'}　{info.require_agent_cert ? '（强制客户端证书）' : '（允许首次换取证书）'}</td></tr>
          </tbody>
        </table>
        <p className="muted" style={{ fontSize: 13 }}>
          指纹绑定的是本套总控的 CA 身份，与地址、域名、证书有效期无关 ——
          服务端换 IP 或换证书都不需要更新 Agent 配置。
        </p>
      </div>

      <div className="card">
        <h3>② 注册密钥 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（Agent 首次接入时使用）</span></h3>
        {info.agent_key_configured ? (
          info.agent_key ? (
            <>
              <div className="row-inline">
                <code className="key-box" title={showKey ? '' : '默认隐藏，点「显示」查看'}>
                  {showKey ? info.agent_key : maskedKey(info.agent_key)}
                </code>
                <button className="btn-mini" onClick={() => setShowKey((v) => !v)}>{showKey ? '隐藏' : '显示'}</button>
                <button className="btn-mini" onClick={() => copy(info.agent_key, 'key')}>
                  {copied === 'key' ? '已复制' : '复制'}
                </button>
              </div>
              <p className="muted" style={{ fontSize: 13 }}>
                密钥只用于<strong>首次换取证书</strong>：Agent 换取成功后会自动把它从配置文件里清掉，
                本机只留下 0600 的证书私钥。因此这条命令只需执行一次。
              </p>
            </>
          ) : (
            <p className="muted">
              注册密钥来自服务端启动参数或环境变量（不在数据目录里），
              请在总控机的 <code>&lt;data&gt;/agent.key</code> 或启动配置中查看。
              （密钥指纹：<code>{info.agent_key_fingerprint || '—'}</code>）
            </p>
          )
        ) : (
          <p className="muted">
            服务端当前未启用注册鉴权（<code>-allow-unauthenticated-agents</code>），
            <strong>任何能连通 Agent 通道的主机都能注册或冒名</strong>，建议尽快关闭该开关。
          </p>
        )}
      </div>

      <div className="card">
        <h3>③ 在新主机上执行</h3>
        <p className="muted" style={{ fontSize: 13 }}>Linux</p>
        <div className="row-inline">
          <code className="cmd-box">{linuxCmd}</code>
          <button className="btn-mini" onClick={() => copy(linuxCmd, 'linux')}>{copied === 'linux' ? '已复制' : '复制'}</button>
        </div>
        <p className="muted" style={{ fontSize: 13 }}>Windows（静默安装为服务）</p>
        <div className="row-inline">
          <code className="cmd-box">{winCmd}</code>
          <button className="btn-mini" onClick={() => copy(winCmd, 'win')}>{copied === 'win' ? '已复制' : '复制'}</button>
        </div>
      </div>

      <div className="card">
        <h3>④ 可选：不用密钥、只认指纹</h3>
        <p className="muted" style={{ fontSize: 13 }}>
          已经持有客户端证书的 Agent，或者想彻底避免在命令行/配置里出现注册密钥时，
          改用公钥指纹校验服务端（服务端换 IP、换证书都不受影响）：
        </p>
        <div className="row-inline">
          <code className="cmd-box">{fpCmd}</code>
          <button className="btn-mini" onClick={() => copy(fpCmd, 'fpc')}>{copied === 'fpc' ? '已复制' : '复制'}</button>
        </div>
      </div>

      <div className="card">
        <h3>常见问题</h3>
        <ul className="muted" style={{ fontSize: 13, lineHeight: 1.9 }}>
          <li><strong>Agent 报"身份指纹不一致"</strong>：服务端重装或换过 CA。删除被控机上的
            <code> creds/</code> 目录并把注册密钥填回配置（<code>enroll-secret</code> 或 <code>agent_key</code>）即可重新换取。</li>
          <li><strong>想彻底禁掉"先换取"</strong>：服务端加 <code>-require-agent-cert</code>，此后只接受已持证 Agent。</li>
          <li><strong>浏览器提示证书不受信任</strong>：Web 控制台默认用自动签发的临时证书（自签名）。
            接受即可，或给服务端配 <code>-web-tls-cert/-web-tls-key</code>，前面挂 TLS 反代并加 <code>-admin-tls-days=0</code>。</li>
          <li><strong>想用外部 CA / 正式证书</strong>：把自备的 <code>ca.crt</code>+<code>ca.key</code> 放进服务端数据目录
            即可复用同一套信任根（也可用 <code>scripts/gencerts</code> 预生成）。</li>
          <li><strong>备份提醒</strong>：服务端重装前请备份 <code>&lt;data&gt;/ca.key</code>，否则所有 Agent 都需要重新换取证书。</li>
        </ul>
      </div>
    </div>
  )
}
