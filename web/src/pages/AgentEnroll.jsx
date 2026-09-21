import React, { useCallback, useEffect, useState } from 'react'
import { Api } from '../api/api.js'

// maskedKey 只露出首尾各 6 位：既能让运维确认"是不是这一把"，
// 又避免密钥长时间明摆在屏幕上（截图/投屏会泄露）。
function maskedKey(key) {
  if (!key) return ''
  if (key.length <= 16) return '•'.repeat(key.length)
  return `${key.slice(0, 6)}${'•'.repeat(16)}${key.slice(-6)}`
}

// fmtTime 秒级时间戳 → 本地可读时间；0 表示"未发生"。
function fmtTime(ts) {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString()
}

const TTL_OPTIONS = [
  { value: 3600, label: '1 小时' },
  { value: 24 * 3600, label: '24 小时' },
  { value: 7 * 24 * 3600, label: '7 天' },
]

// tokenState 与后端的 used_at / expired 两个字段保持一致：
// 用过的要留在账上做迁移盘点，不能从界面上消失。
function tokenState(t) {
  if (t.used_at) return { text: '已使用', cls: 'online' }
  if (t.expired) return { text: '已过期', cls: 'offline' }
  return { text: '待使用', cls: 'idle' }
}

// 一次性注册令牌（一台一份、用后即废）：签发/列举/吊销 + 证书 TOFU 绑定视图。
function EnrollTokens({ issued, setIssued, copied, copy }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')
  const [form, setForm] = useState({ agentId: '', label: '', ttl: 24 * 3600 })
  const [busy, setBusy] = useState(false)

  const reload = useCallback(() => {
    Api.enrollTokens()
      .then((d) => {
        if (d?.ok) { setData(d); setErr('') } else { setErr(d?.msg || '获取注册令牌列表失败') }
      })
      .catch((e) => setErr(e?.response?.data?.msg || e.message || '获取注册令牌列表失败'))
  }, [])

  useEffect(() => { reload() }, [reload])

  const issue = async (e) => {
    e.preventDefault()
    const agentId = form.agentId.trim()
    if (!agentId) { setErr('必须先填写要接入的主机 agent_id'); return }
    setBusy(true)
    try {
      const d = await Api.issueEnrollToken({ agent_id: agentId, label: form.label.trim(), ttl_seconds: Number(form.ttl) })
      if (!d?.ok || !d.token) { setErr(d?.msg || '签发失败'); return }
      // 明文只存在于这一次响应里：界面只显示，不再写进任何列表
      setIssued(d)
      setErr('')
      reload()
    } catch (e2) {
      setErr(e2?.response?.data?.msg || e2.message || '签发失败')
    } finally {
      setBusy(false)
    }
  }

  const revoke = async (payload, tip) => {
    if (!window.confirm(tip)) return
    try {
      const d = await Api.revokeEnrollToken(payload)
      if (!d?.ok) { setErr(d?.msg || '吊销失败'); return }
      setErr('')
      reload()
    } catch (e) {
      setErr(e?.response?.data?.msg || e.message || '吊销失败')
    }
  }

  const resetBinding = async (agentId) => {
    if (!window.confirm(`清除 ${agentId} 的证书绑定？清除后该机下次换取证书必须出示新签发的注册令牌。`)) return
    try {
      const d = await Api.resetEnrollBinding(agentId)
      if (!d?.ok) { setErr(d?.msg || '清除失败'); return }
      setErr('')
      reload()
    } catch (e) {
      setErr(e?.response?.data?.msg || e.message || '清除失败')
    }
  }

  const tokens = data?.tokens || []
  const bindings = data?.bindings || []

  return (
    <div className="card">
      <h3>② 一次性注册令牌 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（推荐：一台一份、用后即废）</span></h3>
      <p className="muted" style={{ fontSize: 13 }}>
        令牌把"谁能从我这里拿到一张合法客户端证书"从"知道部署密钥"收窄成"这台主机的这一次接入动作"：
        它与 <code>agent_id</code> 绑定（A 机的令牌换不到 B 机的身份）、到期作废、换取成功即消耗。
        同时会记住该主机证书的公钥指纹，之后有人拿泄露的共享密钥为该 <code>agent_id</code> 换一把新私钥会被直接拒绝。
      </p>
      {err && <div className="notice err">{err}</div>}

      <form className="row-inline" onSubmit={issue} style={{ flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
        <input style={{ width: 220 }} placeholder="agent_id（如 web-01）" value={form.agentId} autoComplete="off"
          onChange={(e) => setForm({ ...form, agentId: e.target.value })} />
        <input style={{ width: 160 }} placeholder="备注（可选）" value={form.label} autoComplete="off"
          onChange={(e) => setForm({ ...form, label: e.target.value })} />
        <select style={{ width: 120 }} value={form.ttl} onChange={(e) => setForm({ ...form, ttl: e.target.value })}>
          {TTL_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
        <button className="btn btn-sm" type="submit" disabled={busy}>{busy ? '签发中…' : '签发令牌'}</button>
        <button className="btn-mini" type="button" onClick={reload}>刷新</button>
      </form>

      {issued && (
        <div className="notice ok" style={{ marginTop: 12 }}>
          <div>已为 <code>{issued.agent_id}</code> 签发令牌 —— <strong>明文只显示这一次</strong>，关掉本页就再也取不到：</div>
          <div className="row-inline" style={{ marginTop: 6, alignItems: 'center' }}>
            <code className="key-box">{issued.token}</code>
            <button className="btn-mini" onClick={() => copy(issued.token, 'tok')}>{copied === 'tok' ? '已复制' : '复制'}</button>
          </div>
        </div>
      )}

      <div className="table-scroll" style={{ marginTop: 12 }}>
        <table>
          <thead>
            <tr><th>主机</th><th>状态</th><th>备注</th><th>创建</th><th>到期</th><th>操作</th></tr>
          </thead>
          <tbody>
            {tokens.length === 0 && <tr><td colSpan={6} className="table-empty">尚无注册令牌</td></tr>}
            {tokens.map((t) => {
              const st = tokenState(t)
              return (
                <tr key={t.id}>
                  <td><code>{t.agent_id}</code></td>
                  <td><span className={`badge ${st.cls}`}>{st.text}</span></td>
                  <td className="cell-sub">{t.label || '—'}</td>
                  <td className="cell-sub">{fmtTime(t.created_at)}</td>
                  <td className="cell-sub">{t.used_at ? fmtTime(t.used_at) : fmtTime(t.expires_at)}</td>
                  <td>
                    {!t.used_at && (
                      <button className="btn-mini"
                        onClick={() => revoke({ id: t.id }, `吊销这枚 ${t.agent_id} 的令牌？`)}>
                        吊销
                      </button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {tokens.some((t) => !t.used_at && !t.expired) && (
        <p className="muted" style={{ fontSize: 12 }}>
          某台机器不再允许接入时，可一次清掉它全部未使用的令牌：
          {' '}
          <button className="btn-mini" type="button"
            onClick={() => revoke({ agent_id: (form.agentId || '').trim() }, '按上方 agent_id 吊销该机全部未使用令牌？')}>
            按 agent_id 全部吊销
          </button>
        </p>
      )}

      {data && data.shared_key_enabled && (
        <div className="notice err" style={{ marginTop: 12 }}>
          <strong>部署级共享注册密钥仍然可用</strong>：一把钥匙认全网，泄露任意一台即可冒名注册任意主机。
          请让新接入的主机都改用本页签发的一次性令牌；现网全部迁移后，给服务端加
          <code>-disable-shared-agent-key</code> 关闭这条旧路径（当前待使用令牌 {data.pending_tokens || 0} 枚）。
        </div>
      )}

      <h3 style={{ marginTop: 18 }}>已绑定的证书公钥 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（每台主机当前被认可的那把 Agent 私钥）</span></h3>
      <div className="table-scroll">
        <table>
          <thead>
            <tr><th>主机</th><th>公钥指纹</th><th>首次绑定</th><th>重绑次数</th><th>操作</th></tr>
          </thead>
          <tbody>
            {bindings.length === 0 && <tr><td colSpan={5} className="table-empty">尚无绑定（主机首次换取证书时建立）</td></tr>}
            {bindings.map((b) => (
              <tr key={b.agent_id}>
                <td><code>{b.agent_id}</code></td>
                <td><code title={b.spki_hash}>{(b.spki_hash || '').slice(0, 16)}…</code></td>
                <td className="cell-sub">{fmtTime(b.issued_at)}</td>
                <td className="cell-sub">{b.rebinds || 0}</td>
                <td>
                  <button className="btn-mini" onClick={() => resetBinding(b.agent_id)}>清除</button>
                  {(b.rebinds || 0) > 0 && (
                    <span className="cell-sub" style={{ marginLeft: 6 }}>最近重绑 {fmtTime(b.rebound_at)}</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="muted" style={{ fontSize: 12 }}>
        重绑次数只在有人为该 <code>agent_id</code> 出示新令牌时增长 —— 出现非预期的重绑记录，说明有人试过冒名。
      </p>
    </div>
  )
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
  // 刚轮换出来的共享注册密钥：明文只存在于这一次响应里，离开页面即消失
  const [rotated, setRotated] = useState(null)
  const [rotating, setRotating] = useState(false)
  const [keyMsg, setKeyMsg] = useState('')
  // 刚签发的一次性令牌：优先填进下面的接入命令，避免运维再手工粘贴
  const [issued, setIssued] = useState(null)

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

  const rotateSharedKey = async () => {
    const tip = '轮换会生成一把新的共享注册密钥：\n' +
      '· 已凭证书在线的主机不受影响；\n' +
      '· 手里还拿着旧密钥、尚未接入的机器将注册失败，须改用新密钥；\n' +
      '· 新密钥只显示一次。\n\n确定轮换？'
    if (!window.confirm(tip)) return
    setRotating(true)
    setKeyMsg('')
    try {
      const d = await Api.rotateSharedKey()
      if (!d?.ok || !d.agent_key) { setKeyMsg(d?.msg || '轮换失败'); return }
      setRotated(d)
      setShowKey(true)
      // 指纹变了：把接入信息重新拉一次，免得页面继续显示旧指纹
      Api.serverInfo().then(setInfo).catch(() => {})
    } catch (e) {
      setKeyMsg(e?.response?.data?.msg || e.message || '轮换失败')
    } finally {
      setRotating(false)
    }
  }

  if (err) {
    return <div className="page"><div className="card"><p className="muted">获取服务端信息失败：{err}</p></div></div>
  }
  if (!info) {
    return <div className="page"><div className="card"><p className="muted">加载中…</p></div></div>
  }

  const host = (info.listen || '').replace(/^:/, '')
  const server = host && !host.startsWith('0.0.0.0') ? host : '<总控地址>:9000'
  // 只把**刚签发的该主机一次性令牌**放进命令：省掉"复制令牌→粘进命令"这一步。
  // 未签发时留占位符而不是填入共享注册密钥 —— 那把是全网身份根，
  // 出现在可直接粘贴的命令里就等于被贴进工单/聊天/截图，泄露面比密钥本身还大。
  const keyArg = issued?.token || '<一次性注册令牌>'
  const agentIdArg = issued?.agent_id || '<主机名>'
  const linuxCmd = `./mocenter-agent -server ${server} -agentid ${agentIdArg} -agent-key ${keyArg} -daemon`
  const winCmd = `mocenter-deploy.exe install -role agent -server ${server} -agentid ${agentIdArg} -agent-key ${keyArg}`
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

      <EnrollTokens issued={issued} setIssued={setIssued} copied={copied} copy={copy} />

      <div className="card">
        <h3>③ 共享注册密钥 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（旧方式，仅迁移过渡期保留）</span></h3>
        {info.shared_key_enabled ? (
          <>
            <table className="kv">
              <tbody>
                <tr><th>密钥指纹</th><td><code>{info.agent_key_fingerprint || '—'}</code></td></tr>
                <tr><th>所在文件</th><td><code>{info.agent_key_path || '<数据目录>/agent.key'}</code></td></tr>
              </tbody>
            </table>
            {rotated ? (
              <>
                <div className="row-inline" style={{ marginTop: 8 }}>
                  <code className="key-box" title="只显示这一次，离开页面后无法再查看">
                    {showKey ? rotated.agent_key : maskedKey(rotated.agent_key)}
                  </code>
                  <button className="btn-mini" onClick={() => setShowKey((v) => !v)}>{showKey ? '隐藏' : '显示'}</button>
                  <button className="btn-mini" onClick={() => copy(rotated.agent_key, 'key')}>
                    {copied === 'key' ? '已复制' : '复制'}
                  </button>
                </div>
                <ul className="muted" style={{ fontSize: 13, lineHeight: 1.9 }}>
                  {(rotated.warnings || []).map((x) => <li key={x}>{x}</li>)}
                </ul>
              </>
            ) : (
              <p className="muted" style={{ fontSize: 13 }}>
                本页不再显示密钥明文：它是全网主机的身份根，而过去每次打开控制台都会把它随页面一起带走
                （截图、浏览器扩展、反代日志任一环节都能捡到）。需要给一台还没迁到一次性令牌的旧机器
                配密钥时，点下面的「轮换」拿一把新的。
              </p>
            )}
            <div className="row-inline">
              <button className="btn" onClick={rotateSharedKey} disabled={rotating}>
                {rotating ? '轮换中…' : (rotated ? '再轮换一次' : '轮换共享密钥')}
              </button>
              {keyMsg && <span className="muted">{keyMsg}</span>}
            </div>
            <p className="muted" style={{ fontSize: 13 }}>
              这把密钥<strong>一把认全网</strong>：拿到它的任何主机都能以任意 <code>agent_id</code> 换取证书，
              泄露一台即等于全网身份泄露。它只为"现网未迁移的主机还能上线"保留一个版本，
              新接入请用上方的<strong>一次性注册令牌</strong>；全部迁移后给服务端加
              <code>-disable-shared-agent-key</code> 关掉这条路径。
            </p>
            <p className="muted" style={{ fontSize: 13 }}>
              凭据只用于<strong>首次换取证书</strong>：Agent 换取成功后会自动把它从配置文件里清掉，
              本机只留下 0600 的证书私钥。因此这条命令只需执行一次。
            </p>
          </>
        ) : (
          <p className="muted">
            共享注册密钥这条路径已关闭，只有上方签发的一次性注册令牌能换取证书
            （若服务端连令牌存储也没有，则等同于 <code>-allow-unauthenticated-agents</code>：
            <strong>任何能连通 Agent 通道的主机都能注册或冒名</strong>，请务必确认不是这种状态）。
          </p>
        )}
      </div>

      <div className="card">
        <h3>④ 在新主机上执行</h3>
        <p className="muted" style={{ fontSize: 13 }}>
          先在 ② 里为该主机签发令牌，下面的命令会自动带上它（令牌与主机名一起替换进命令）。
        </p>
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
        {issued && (
          <p className="muted" style={{ fontSize: 12 }}>
            当前命令里填的是刚为 <code>{issued.agent_id}</code> 签发的令牌（<code>-agentid</code> 已同步成该主机）。
          </p>
        )}
      </div>

      <div className="card">
        <h3>⑤ 可选：不用凭据、只认指纹</h3>
        <p className="muted" style={{ fontSize: 13 }}>
          已经持有客户端证书的 Agent，或者想彻底避免在命令行/配置里出现注册凭据时，
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
          <li><strong>Agent 报"身份指纹不一致" / "服务端部署身份 CA 已变化"</strong>：服务端重装或换过 CA。
            把本页显示的<strong>新指纹</strong>填进被控机的 <code>server_fingerprint</code>（写在
            <code>mocenter-agent.json</code> 里远程下发即可），Agent 会<strong>自动</strong>重新换取证书，无需登录被控机。
            没填指纹时 Agent 会拒绝自动重换并给出新旧指纹 —— 这是刻意设计：避免把注册凭据交给陌生对端。
            自动重换会<strong>复用</strong>被控机的 <code>creds/client.key</code>（公钥不变，绑定才认这台机器）；
            手工恢复时也只删 <code>creds/client.crt</code> 与 <code>creds/ca.crt</code>，把注册凭据填回
            <code>agent_key</code>。整目录删掉等于换了一把新私钥，那时必须再签一枚一次性令牌。</li>
          <li><strong>想彻底禁掉"先换取"</strong>：服务端加 <code>-require-agent-cert</code>，此后只接受已持证 Agent。</li>
          <li><strong>担心共享注册密钥泄露</strong>：点 ③ 的「轮换共享密钥」换一把 —— 旧密钥此后对新注册立即失效
            （已凭证书在线的主机不掉线）。新接入改用上方的一次性注册令牌（一台一份、用过即废）。
            已经绑过证书的主机，被人拿泄露的密钥换一把新私钥时会被"公钥绑定"当场拒掉并写入审计（类别 auth）；
            恢复办法是给那台真机签发一枚新令牌（等于运维显式授权重绑），或点「清除」绑定后重新接入。
            现网全部迁到令牌后，给服务端加 <code>-disable-shared-agent-key</code> 关掉旧路径。</li>
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
