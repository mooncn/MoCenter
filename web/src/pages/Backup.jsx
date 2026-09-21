import React, { useEffect, useState } from 'react'
import { Api } from '../api/api.js'

// 备份 / 恢复页（P1）。
//
// 交互上刻意做了两道"减速带"，因为这两件事都不可逆：
//
//   - 恢复前必须先看清单（inspect）：让运维确认"这个包里是哪个版本、有哪些文件"，
//     而不是凭文件名直接点恢复；
//   - 恢复需要手动输入 "RESTORE" 才能执行，且明确提示要重启服务端。
//
// 备份包内**默认不含 Agent 注册密钥**（勾选"包含凭据文件"才会打包），但仍含账号口令哈希
// 与 TOTP 密钥，页面会把这一点写出来，避免运维当成普通配置文件随手分享出去。
export default function Backup() {
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState('')
  const [msg, setMsg] = useState(null)
  const [manifest, setManifest] = useState(null)
  const [manifestFile, setManifestFile] = useState('')
  const [confirmName, setConfirmName] = useState('')
  const [version, setVersion] = useState(null)
  // 注册密钥是"一把认全网"的身份根，而备份包会被下载到处流转 —— 默认不带。
  const [withSecrets, setWithSecrets] = useState(false)

  const load = () => {
    setLoading(true)
    Api.backups()
      .then((d) => setItems(Array.isArray(d.items) ? d.items : []))
      .catch((e) => setMsg({ type: 'err', text: '加载备份列表失败: ' + (e.response?.data?.msg || e.message) }))
      .finally(() => setLoading(false))
  }

  useEffect(() => {
    load()
    Api.version().then(setVersion).catch(() => {})
  }, [])

  const create = async () => {
    setBusy('create')
    setMsg(null)
    try {
      const d = await Api.createBackup(withSecrets)
      setMsg({
        type: d.ok ? 'ok' : 'err',
        text: d.ok
          ? `备份已创建：${d.file}（${fmtBytes(d.size)}，${d.files} 个文件，耗时 ${d.duration}）。` +
            (withSecrets ? '本次已应要求打入凭据文件。' : '凭据文件（agent.key）默认未打入，需要它请勾选后再备一次。')
          : (d.msg || '创建失败'),
      })
      load()
    } catch (e) {
      setMsg({ type: 'err', text: '创建备份失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setBusy('')
    }
  }

  const inspect = async (name) => {
    setBusy('inspect:' + name)
    setManifest(null)
    setMsg(null)
    try {
      const d = await Api.inspectBackup(name)
      if (d.ok) {
        setManifest(d.manifest)
        setManifestFile(name)
        setConfirmName('')
      } else {
        setMsg({ type: 'err', text: d.msg || '读取清单失败' })
      }
    } catch (e) {
      setMsg({ type: 'err', text: '读取清单失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setBusy('')
    }
  }

  const restore = async (name) => {
    if (confirmName !== 'RESTORE') {
      setMsg({ type: 'err', text: '请输入 RESTORE 以确认恢复操作' })
      return
    }
    setBusy('restore:' + name)
    setMsg(null)
    try {
      const d = await Api.restoreBackup(name)
      setMsg({
        type: d.ok ? 'ok' : 'err',
        text: d.ok
          ? `恢复完成（${d.restored?.length || 0} 个文件）。请**重启服务端**使配置与账号生效。`
          : (d.msg || '恢复失败'),
      })
      setManifest(null)
      load()
    } catch (e) {
      setMsg({ type: 'err', text: '恢复失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setBusy('')
    }
  }

  const download = async (name) => {
    setBusy('download:' + name)
    try {
      const blob = await Api.downloadBackup(name)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = name
      document.body.appendChild(a)
      a.click()
      a.remove()
      // 立即释放会中断下载，延后回收
      setTimeout(() => URL.revokeObjectURL(url), 60000)
    } catch (e) {
      setMsg({ type: 'err', text: '下载失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setBusy('')
    }
  }

  const remove = async (name) => {
    if (!window.confirm(`确认删除备份 ${name}？此操作不可撤销。`)) return
    setBusy('delete:' + name)
    try {
      const d = await Api.deleteBackup(name)
      setMsg({ type: d.ok ? 'ok' : 'err', text: d.msg || (d.ok ? '已删除' : '删除失败') })
      load()
    } catch (e) {
      setMsg({ type: 'err', text: '删除失败: ' + (e.response?.data?.msg || e.message) })
    } finally {
      setBusy('')
    }
  }

  return (
    <div>
      <div className="card">
        <div className="lat-head">
          <h3>数据备份</h3>
          <button className="btn" onClick={create} disabled={busy === 'create'}>
            {busy === 'create' ? '备份中…' : '立即备份'}
          </button>
        </div>
        <label className="checkbox-item" style={{ display: 'inline-flex', alignItems: 'center', gap: 6, marginTop: 8 }}>
          <input type="checkbox" checked={withSecrets} onChange={(e) => setWithSecrets(e.target.checked)} />
          包含凭据文件 <code>agent.key</code>（整机迁移时才需要）
        </label>
        <p className="cell-sub">
          备份内容：主机与历史数据、账号、备注、展示配置。<strong>默认不含</strong> <code>agent.key</code>
          （Agent 注册密钥是一把认全网的身份根，而备份包会被下载到处流转）；不含安装包与日志。
          包里仍有账号口令哈希与 TOTP 密钥，属凭据，请妥善保管，不要随意外发。
          {version?.server_version ? ` 当前版本 ${version.server_version}。` : ''}
        </p>
        {msg && <div className={`lat-msg ${msg.type}`}>{msg.text}</div>}
      </div>

      <div className="card">
        <h3>已有备份</h3>
        {loading
          ? <div className="empty">加载中…</div>
          : items.length === 0
            ? <div className="empty">还没有备份。点击上方「立即备份」创建第一个。</div>
            : (
              <table className="table">
                <thead>
                  <tr>
                    <th>文件名</th><th>大小</th><th>创建时间</th><th>版本</th><th>文件数</th><th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((it) => (
                    <tr key={it.name}>
                      <td className="mono">{it.name}{it.error && <span className="lat-loss"> （包异常）</span>}</td>
                      <td>{fmtBytes(it.size)}</td>
                      <td>{new Date(it.mod_time * 1000).toLocaleString()}</td>
                      <td>{it.version || '—'}</td>
                      <td>{it.files ?? '—'}</td>
                      <td className="row-actions">
                        <button className="btn ghost sm" onClick={() => inspect(it.name)}
                          disabled={busy === 'inspect:' + it.name}>查看清单</button>
                        <button className="btn ghost sm" onClick={() => download(it.name)}
                          disabled={busy === 'download:' + it.name}>下载</button>
                        <button className="btn ghost sm danger" onClick={() => remove(it.name)}
                          disabled={busy === 'delete:' + it.name}>删除</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
      </div>

      {manifest && (
        <div className="card">
          <div className="public-card-title">
            <h3>备份清单 · {manifestFile}</h3>
            <button className="btn ghost sm" onClick={() => setManifest(null)}>关闭</button>
          </div>
          <div className="public-facts">
            <div className="public-fact"><span className="k">格式版本</span><span className="v">{manifest.format_version}</span></div>
            <div className="public-fact"><span className="k">生成时间</span><span className="v">{manifest.created_at_text}</span></div>
            <div className="public-fact"><span className="k">服务端版本</span><span className="v">{manifest.app_version || '—'}</span></div>
            <div className="public-fact"><span className="k">来源主机</span><span className="v">{manifest.hostname || '—'}</span></div>
          </div>
          <table className="table" style={{ marginTop: 12 }}>
            <thead>
              <tr><th>文件</th><th>说明</th><th>大小</th><th>校验</th></tr>
            </thead>
            <tbody>
              {(manifest.entries || []).map((e) => (
                <tr key={e.name}>
                  <td className="mono">{e.name}</td>
                  <td>
                    {e.label}
                    {e.sensitive && <span className="lat-badge">含凭据</span>}
                  </td>
                  <td>{fmtBytes(e.size)}</td>
                  <td className="cell-sub mono">{(e.sha256 || '').slice(0, 12)}…</td>
                </tr>
              ))}
            </tbody>
          </table>
          {(manifest.skipped || []).length > 0 && (
            <div className="lat-msg warn">
              包内未包含：{manifest.skipped.join('、')}
            </div>
          )}

          <div className="restore-box">
            <div className="restore-title">从该备份恢复</div>
            <p className="cell-sub">
              恢复会**覆盖**当前数据（被覆盖的文件会另存为 <code>*.before-restore</code>）。
              恢复完成后需要**重启服务端**才能让配置与账号生效。
            </p>
            <div className="restore-actions">
              <input
                value={confirmName}
                onChange={(e) => setConfirmName(e.target.value)}
                placeholder="输入 RESTORE 以确认"
              />
              <button
                className="btn danger"
                onClick={() => restore(manifestFile)}
                disabled={confirmName !== 'RESTORE' || busy === 'restore:' + manifestFile}
              >
                {busy === 'restore:' + manifestFile ? '恢复中…' : '执行恢复'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function fmtBytes(n) {
  if (n === undefined || n === null || n === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n, i = 0
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}
