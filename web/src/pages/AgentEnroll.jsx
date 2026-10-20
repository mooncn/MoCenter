import React, { useCallback, useEffect, useState } from 'react'
import { Api } from '../api/api.js'
import Pagination from '../components/Pagination.jsx'

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
  // 已登记主机的 agent_id：给签发表单做候选。重装/重接入时直接选原名称，
  // 避免"看起来像"的手填值把令牌签给另一台主机。
  const [knownIDs, setKnownIDs] = useState([])
  // 令牌会随接入/重装不断累积，列表分页显示，避免整页被历史记录撑长。
  const [tokenPage, setTokenPage] = useState(1)
  const [tokenPageSize, setTokenPageSize] = useState(10)

  const loadKnown = useCallback(() => {
    Api.agents()
      .then((d) => setKnownIDs((Array.isArray(d) ? d : []).map((a) => a.agent_id).filter(Boolean)))
      .catch(() => {})
  }, [])

  const reload = useCallback(() => {
    Api.enrollTokens()
      .then((d) => {
        if (d?.ok) { setData(d); setErr('') } else { setErr(d?.msg || '获取注册令牌列表失败') }
      })
      .catch((e) => setErr(e?.response?.data?.msg || e.message || '获取注册令牌列表失败'))
    // 主机是否在册决定"删除已注销令牌"按钮是否出现，和令牌列表一起刷新
    loadKnown()
  }, [loadKnown])

  useEffect(() => { reload() }, [reload])

  // 仪表盘删除主机后会派发该事件（同页打开时也能即时更新）
  useEffect(() => {
    window.addEventListener('mc-agents-refresh', loadKnown)
    return () => window.removeEventListener('mc-agents-refresh', loadKnown)
  }, [loadKnown])

  const issue = async (e) => {
    e.preventDefault()
    const agentId = form.agentId.trim()
    // 不绑定令牌是"一次性通行证"：谁先拿到谁就能用，且能覆盖同名主机的证书绑定。
    // 留空是合法用法，但必须让人在签发前明确知道自己在发什么。
    if (!agentId && !window.confirm(
      '未填写 agent_id：这枚令牌不绑定任何主机，任何拿到它的主机都能用它换取一次证书，' +
      '身份由该机决定（未指定时默认机器 UUID）。\n\n' +
      '请只通过可信渠道交付。确定签发不绑定令牌？')) return
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

  // 永久删除"已注销主机"的令牌记录：与吊销不同，这是清理账本 ——
  // 主机已经不在册，这条令牌不再对应任何可接入对象。
  const removeToken = async (t) => {
    if (!window.confirm(
      `永久删除 ${t.agent_id} 的这条令牌记录？\n\n` +
      '仅适用于已从服务端注销、且曾经完成过证书换取的主机。删除后无法恢复；' +
      '若该机以后还要接入，需重新签发令牌。'
    )) return
    try {
      const d = await Api.deleteEnrollToken(t.id)
      if (!d?.ok) { setErr(d?.msg || '删除失败'); return }
      setErr('')
      reload()
    } catch (e) {
      setErr(e?.response?.data?.msg || e.message || '删除失败')
    }
  }

  const tokens = data?.tokens || []
  const bindings = data?.bindings || []
  // 主机已注销（不在当前登记列表里）但留下过证书绑定 = 确实接入过、后来被删了。
  // 这类令牌才允许删除；从未接入过的令牌只能吊销（避免误删还等着用的凭据）。
  const boundIDs = new Set(bindings.map((b) => b.agent_id))
  const goneHost = (t) => !t.unbound && !!t.agent_id && !knownIDs.includes(t.agent_id) && boundIDs.has(t.agent_id)

  // 页码收敛在渲染期算：删除/吊销后令牌变少时不会先渲染一屏空白
  const tokenTotalPages = Math.max(1, Math.ceil(tokens.length / tokenPageSize))
  const tokenCurPage = Math.min(Math.max(1, tokenPage), tokenTotalPages)
  const pagedTokens = tokens.slice((tokenCurPage - 1) * tokenPageSize, tokenCurPage * tokenPageSize)

  return (
    <div className="card">
      <h3>② 一次性注册令牌 <span className="muted" style={{ fontWeight: 'normal', fontSize: 13 }}>（agent_id 选填；用后即废）</span></h3>
      <p className="muted" style={{ fontSize: 13 }}>
        令牌把"谁能从我这里拿到一张合法客户端证书"从"知道部署密钥"收窄成"这台主机的这一次接入动作"：
        <code>agent_id</code> <strong>选填</strong> —— 填写时令牌只认该主机（A 机的令牌换不到 B 机的身份），
        留空则不绑定主机、任何主机可用一次（身份由该机决定，未指定时默认机器 UUID）。
        两种令牌都到期作废、换取成功即消耗。服务端还会记住该主机证书的公钥指纹，
        之后有人拿泄露的共享密钥为该 <code>agent_id</code> 换一把新私钥会被直接拒绝。
      </p>
      <div className="notice" style={{ background: 'var(--panel-2)', color: 'var(--muted)' }}>
        <strong>绑定令牌只认签发时填写的 agent_id；留空则谁先拿到谁就能用一次。</strong>
        Agent 启动时若没有指定 <code>-agentid</code>（Linux）或安装向导里的「Agent 名称/ID」（Windows），
        会回退成<strong>机器硬件 UUID</strong>（探测不到才用主机名）。绑定令牌与名称不一致时，服务端只会报
        「注册凭证校验失败」—— 名称不匹配<strong>不会消耗令牌</strong>，改成一致的名称后可直接重试。
        不绑定令牌没有这个限制，但它是一次性通行证，请只通过可信渠道交付。
        {knownIDs.length > 0 && ' 下方输入框会提示已登记主机的名称。'}
      </div>
      {err && <div className="notice err">{err}</div>}

      <form className="row-inline" onSubmit={issue} style={{ flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
        <input style={{ width: 220 }} placeholder="agent_id（选填，留空=不绑定）" value={form.agentId} autoComplete="off"
          list="enroll-known-ids"
          onChange={(e) => setForm({ ...form, agentId: e.target.value })} />
        <datalist id="enroll-known-ids">
          {knownIDs.map((id) => <option key={id} value={id} />)}
        </datalist>
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
          <div>
            {issued.unbound
              ? <>已签发<strong>不绑定</strong>令牌（任何主机一次，身份按该机上报）</>
              : <>已为 <code>{issued.agent_id}</code> 签发令牌</>}
            {' '}—— <strong>明文只显示这一次</strong>，关掉本页就再也取不到：
          </div>
          <div className="row-inline" style={{ marginTop: 6, alignItems: 'center' }}>
            <code className="key-box">{issued.token}</code>
            <button className="btn-mini" onClick={() => copy(issued.token, 'tok')}>{copied === 'tok' ? '已复制' : '复制'}</button>
          </div>
        </div>
      )}

      <div className="table-scroll" style={{ marginTop: 12 }}>
        <table>
          <thead>
            <tr><th>绑定主机</th><th>状态</th><th>备注</th><th>创建</th><th>到期</th><th>操作</th></tr>
          </thead>
          <tbody>
            {tokens.length === 0 && <tr><td colSpan={6} className="table-empty">尚无注册令牌</td></tr>}
            {pagedTokens.map((t) => {
              const st = tokenState(t)
              return (
                <tr key={t.id}>
                  <td>{t.unbound ? <span className="muted">不绑定（按该机上报身份）</span> : <code>{t.agent_id}</code>}</td>
                  <td><span className={`badge ${st.cls}`}>{st.text}</span></td>
                  <td className="cell-sub">{t.label || '—'}</td>
                  <td className="cell-sub">{fmtTime(t.created_at)}</td>
                  <td className="cell-sub">{t.used_at ? fmtTime(t.used_at) : fmtTime(t.expires_at)}</td>
                  <td>
                    <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
                      {!t.used_at && (
                        <button className="btn-mini"
                          onClick={() => revoke({ id: t.id }, `吊销这枚 ${t.unbound ? '不绑定' : t.agent_id} 的令牌？`)}>
                          吊销
                        </button>
                      )}
                      {goneHost(t) && (
                        <button className="secondary danger btn-mini"
                          title="该主机已注销且曾完成证书换取：永久删除这条令牌记录"
                          onClick={() => removeToken(t)}>
                          删除记录
                        </button>
                      )}
                    </span>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      <Pagination
        total={tokens.length}
        page={tokenCurPage}
        pageSize={tokenPageSize}
        onPage={setTokenPage}
        onPageSize={(n) => { setTokenPageSize(n); setTokenPage(1) }}
      />
      {tokens.some(goneHost) && (
        <p className="muted" style={{ fontSize: 12 }}>
          「删除记录」只出现在<strong>已注销主机</strong>的令牌上（该主机曾被删除，且留下过证书绑定）。
          主机仍在册、或令牌签发后从未接入过时，请用「吊销」。
        </p>
      )}
      {form.agentId.trim() && tokens.some((t) => !t.used_at && !t.expired && !t.unbound && t.agent_id === form.agentId.trim()) && (
        <p className="muted" style={{ fontSize: 12 }}>
          某台机器不再允许接入时，可一次清掉它全部未使用的令牌：
          {' '}
          <button className="btn-mini" type="button"
            onClick={() => revoke({ agent_id: form.agentId.trim() }, '按上方 agent_id 吊销该机全部未使用令牌？')}>
            按 agent_id 全部吊销
          </button>
        </p>
      )}
      {tokens.some((t) => t.unbound && !t.used_at && !t.expired) && (
        <p className="muted" style={{ fontSize: 12 }}>
          不绑定令牌不对应任何主机，无法按 agent_id 批量吊销 —— 请在表格里逐条吊销。
        </p>
      )}
      <p className="muted" style={{ fontSize: 12 }}>
        令牌只在<strong>换取客户端证书</strong>时消耗。已经持有证书的机器重连只出示证书，
        不会再消耗令牌 —— 若命令行粘贴了令牌但仍显示「待使用」，说明该机
        <code>creds/</code> 里已有可用证书，删掉 <code>client.crt</code> 与 <code>ca.crt</code>
        （保留 <code>client.key</code>）后重启即可重新换取。
      </p>

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

  // 从 -listen 拆出主机与端口，并把「通配/空主机」补成服务端探测到的公网 IP。
  //
  // 为什么必须这么拼：-listen 的常见写法是 ":9000" 或 "0.0.0.0:9000"，两者都没有可用主机名。
  // 原实现只把开头的 ':' 去掉，于是 ":9000" 会变成 "9000" —— 生成的命令成了
  // `-server 9000`，Agent 拿去连必然失败，而运维从命令表面看不出哪里不对。
  //
  // 端口必须取自 -listen（Agent TCP 端口），**不能**用 location.host：
  // 那是控制台的 HTTP 端口（如 8080），Agent 连过去会连到 Web 服务上。
  const listenPort = (info.listen || '').match(/:(\d+)\s*$/)?.[1] || '9000'
  const listenHost = (info.listen || '').replace(/:\d+\s*$/, '').replace(/^\[|\]$/g, '').trim()
  const wildcardHost = !listenHost || listenHost === '0.0.0.0' || listenHost === '::' || listenHost === '*'
  // 探测不到公网 IP 时退回占位符，而不是拼一个连不上的地址
  const serverHost = wildcardHost ? (info.public_ip || '') : listenHost
  const server = serverHost ? `${serverHost}:${listenPort}` : `<总控地址>:${listenPort}`
  // 只把**刚签发的该主机一次性令牌**放进命令：省掉"复制令牌→粘进命令"这一步。
  // 未签发时留占位符而不是填入共享注册密钥 —— 那把是全网身份根，
  // 出现在可直接粘贴的命令里就等于被贴进工单/聊天/截图，泄露面比密钥本身还大。
  const keyArg = issued?.token || '<一次性注册令牌>'
  // 不绑定令牌不带 -agentid：让 Agent 走默认身份（未指定时回退机器 UUID）；
  // 绑定令牌则必须原样带上签发时的名称，否则服务端会拒绝。
  const agentIdArg = issued ? (issued.unbound ? '' : issued.agent_id) : '<主机名>'
  const agentIdFlag = agentIdArg ? ` -agentid ${agentIdArg}` : ''
  const serviceAgentId = agentIdArg ? ` --agentid=${agentIdArg}` : ''
  // Linux：先用前台进程完成首次换取（看到上线后 Ctrl+C），再交给 systemd。
  // 两步分开写是因为 install-linux.sh 不接受注册凭据：先手工换取，
  // 证书落到 /etc/mocenter/creds，之后服务只凭证书启动，凭据不再落盘。
  const linuxEnrollCmd = [
    'sudo install -m 0755 ./mocenter-agent /usr/local/bin/mocenter-agent',
    'sudo mkdir -p /etc/mocenter && sudo chmod 700 /etc/mocenter',
    `sudo /usr/local/bin/mocenter-agent -config /etc/mocenter/mocenter-agent.json -server ${server}${agentIdFlag} -enroll-secret ${keyArg}`,
  ].join('\n')
  const linuxServiceCmd = `sudo bash scripts/deploy/install-linux.sh --role=agent --bin=/usr/local/bin/mocenter-agent --server=${server}${serviceAgentId}`
  // Windows：图形向导（推荐）或静默命令。凭据走临时文件，安装器读完即删，
  // 不进命令行 —— 安装那几十秒里同机任何进程都能看到别人的命令行。
  const winCmd = `mocenter-deploy.exe install -role agent -server ${server}${agentIdFlag} -agent-key-file C:\\Windows\\Temp\\mc-token.txt`
  const fpCmd = `./mocenter-agent -server ${server} -agentid <主机名> -server-fingerprint ${info.fingerprint || '<指纹>'}`

  return (
    <div className="page">
      <h2 className="page-title">Agent 接入</h2>
      <p className="muted" style={{ marginTop: -6 }}>
        接入分三步：签发一次性令牌（<code>agent_id</code> 可选）→ 在被控机安装 Agent（Linux / Windows 分别见 ④）→ 回仪表盘确认上线。
        加密与双向认证自动完成，无需在本机准备任何证书文件。
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
        <h3>④ 在新主机上安装 Agent</h3>
        <p className="muted" style={{ fontSize: 13 }}>
          先在 ② 里签发令牌，下面的命令会自动带上它：绑定令牌连 <code>agent_id</code> 一起写进命令，
          不绑定令牌则不带名称、由该机自行决定身份。命令里的端口是 Agent 通道（服务端 <code>-listen</code>，默认 9000），<strong>不是</strong>浏览器访问控制台的 8080 ——
          写反的表现是 Agent 一直重连、仪表盘始终不上线。
        </p>
        {!serverHost && (
          <p className="muted" style={{ fontSize: 13 }}>
            ⚠ 未探测到本机公网 IP（服务端需能访问外网回显服务），命令里的 &lt;总控地址&gt;
            请手工替换成被控机能访问到的地址。
          </p>
        )}
        <p className="muted" style={{ fontSize: 12 }}>
          {issued ? (
            issued.unbound ? (
              <>当前命令填的是刚签发的<strong>不绑定</strong>令牌：命令不带 <code>-agentid</code>／「Agent 名称/ID」，
                Agent 会以<strong>机器硬件 UUID</strong> 上线。若想用自定义名称，自行加上
                <code>-agentid &lt;名称&gt;</code> 即可（无需与令牌一致）。</>
            ) : (
              <>当前命令填的是刚为 <code>{issued.agent_id}</code> 签发的令牌；安装时的
                <code>-agentid</code>／「Agent 名称/ID」必须与此<strong>完全一致</strong>。</>
            )
          ) : (
            <>还没有签发令牌：命令里是占位符。先完成 ② 再复制，或手工把占位符替换成令牌与主机名。</>
          )}
        </p>

        <h4 style={{ marginBottom: 4 }}>Linux</h4>
        <p className="muted" style={{ fontSize: 13 }}>
          <strong>第 1 步 · 首次换取证书（只做一次）</strong>：把 <code>mocenter-agent</code> 拷到目标机，执行下面的命令。
          日志出现「已从服务端换取客户端证书」、仪表盘显示在线后，按 <code>Ctrl+C</code> 结束前台进程。
          一次性令牌用 <code>-enroll-secret</code> 传入；只有仍在使用部署级共享密钥的旧机器才用
          <code>-agent-key</code>（两者的区别见 ③）。
        </p>
        <div className="row-inline" style={{ alignItems: 'flex-start' }}>
          <pre className="cmd-box" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', margin: 0 }}>{linuxEnrollCmd}</pre>
          <button className="btn-mini" onClick={() => copy(linuxEnrollCmd, 'linux1')}>{copied === 'linux1' ? '已复制' : '复制'}</button>
        </div>
        <p className="muted" style={{ fontSize: 13 }}>
          <strong>第 2 步 · 注册为 systemd 服务</strong>：证书已在 <code>/etc/mocenter/creds</code>，服务只凭证书启动，
          不再需要令牌。
        </p>
        <div className="row-inline">
          <code className="cmd-box">{linuxServiceCmd}</code>
          <button className="btn-mini" onClick={() => copy(linuxServiceCmd, 'linux2')}>{copied === 'linux2' ? '已复制' : '复制'}</button>
        </div>
        <p className="muted" style={{ fontSize: 13 }}>
          <strong>第 3 步 · 确认</strong>：<code>systemctl status mocenter-agent --no-pager</code> 为 active；
          <code>sudo ls -l /etc/mocenter/creds/</code> 能看到 <code>client.crt</code>／<code>client.key</code>；
          日志用 <code>journalctl -u mocenter-agent -n 50 --no-pager</code> 查看。
        </p>

        <h4 style={{ marginBottom: 4 }}>Windows</h4>
        <p className="muted" style={{ fontSize: 13, marginTop: 4 }}>
          <strong>方式 A · 图形安装向导（推荐）</strong>：以管理员身份运行 <code>MoCenter-Setup-*.exe</code>，
          角色选 <strong>Agent</strong>，填写服务端地址与一次性注册令牌。
          「Agent 名称/ID」：绑定令牌<strong>必须</strong>填 ② 里签发时用的 <code>agent_id</code>；
          不绑定令牌可留空（默认机器 UUID），也可自定名称。
          若向导里没有「Agent 名称/ID」这一项（1.0.2 及更早版本）：不绑定令牌可直接用；
          绑定令牌请改用方式 B，否则 Agent 会以机器 UUID 上报，令牌必然校验失败。
          已经用旧版装过的机器，用新版安装包<strong>覆盖安装</strong>即可：升级会把本次填写的「Agent 名称/ID」与令牌
          写进既有配置，并在身份变化时清掉旧证书（保留 <code>client.key</code>）后重新换取；
          其余配置项留空即沿用原值。
        </p>
        <p className="muted" style={{ fontSize: 13 }}>
          <strong>方式 B · 命令行静默安装</strong>：先把令牌<strong>单独一行</strong>写进
          <code>C:\Windows\Temp\mc-token.txt</code>，再以管理员身份执行（安装器读完会自动删除该文件，
          令牌不进命令行历史）：
        </p>
        <div className="row-inline">
          <code className="cmd-box">{winCmd}</code>
          <button className="btn-mini" onClick={() => copy(winCmd, 'win')}>{copied === 'win' ? '已复制' : '复制'}</button>
        </div>
        <p className="muted" style={{ fontSize: 13 }}>
          <strong>第 3 步 · 确认</strong>：<code>sc query MoCenterAgent</code> 为 <code>RUNNING</code>；
          配置在 <code>C:\ProgramData\MoCenter\agent\mocenter-agent.json</code>（<code>enrollment_secret</code>
          换取成功后被清空），证书在 <code>C:\ProgramData\MoCenter\agent\creds\</code>；
          日志在 <code>C:\ProgramData\MoCenter\agent\logs\MoCenterAgent.log</code>。
          安装失败时先看引擎日志 <code>&lt;安装目录&gt;\mocenter-deploy.log</code>。
        </p>
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
          <li><strong>令牌签发后一直显示「待使用」</strong>：先看目标机日志里有没有
            「本机已有客户端证书……注册凭据本次不会被使用」。有证书的 Agent 重连只出示证书，
            不会再消耗令牌（这是正常的）；要改用令牌重换，请删除该机 <code>creds/client.crt</code>
            与 <code>creds/ca.crt</code>、<strong>保留 <code>client.key</code></strong> 后重启。
            若日志里是「服务端拒绝换取客户端证书（本机 agent_id=…）」，则按上一条核对名称；
            名称不匹配同样<strong>不会消耗令牌</strong>。</li>
          <li><strong>Agent 报「注册凭证校验失败」/「令牌无效」</strong>：若令牌签发时 <code>agent_id</code> 留空
            （不绑定令牌），不会因名称不一致被拒，可跳过下面的名称核对，先确认令牌未过期、未被使用。
            绑定令牌则先核对 <code>agent_id</code>，而不是怀疑令牌抄错。
            Agent 未指定名称时会回退成<strong>机器硬件 UUID</strong>（Windows 安装向导 1.0.2 及更早没有名称项），
            与签发令牌时填写的名字不一致，服务端就无法区分"名字不匹配"与"凭据不对"（刻意不给出探测口）。
            在目标机日志里能找到实际使用的名称（开头的「[agent] 启动 …，Agent …」或
            「未指定 AgentID，使用设备 UUID」）；按该名称重签令牌，
            或在启动参数／安装向导里显式指定签发时的名称即可。名称不匹配<strong>不会消耗令牌</strong>。
            若令牌已过期或已使用，重新签发一枚。</li>
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
