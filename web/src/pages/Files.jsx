import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Api } from '../api/api.js'
import Modal from '../components/Modal.jsx'
import { hostLabel } from '../utils/hostName.js'

// 文件管理：按会话浏览并维护 Agent 主机上的整机文件。
//
// 口径（与服务端一致）：
//   - 整机访问，不再限制在 -file-roots 沙箱内；默认根为 /（Windows 是盘符列表）；
//   - 列出 / 新建 / 重命名 / 复制 / 移动 / 删除 / 上传 / 下载，全部以 Agent 服务账号权限执行；
//   - 复制/剪切走"剪贴板 + 粘贴"，与资源管理器一致；
//   - 会话无操作 20 分钟自动断开（没有累计时长上限），离开页面时主动关闭。

// 剪贴板最多保存 100 项：再多就不该靠界面搬运，直接用命令行或文件分发更合适。
const CLIP_MAX = 100
// 剪贴板列表分页大小（用户要求"可展示列表可分页"）。
const CLIP_PAGE = 10

// 可在浏览器里预览/编辑的文本格式：其余文件只提供下载，
// 避免把二进制当文本打开（Agent 侧还会再挡一次 NUL/控制字符）。
const TEXT_EXTS = new Set([
  'log', 'txt', 'text', 'md', 'markdown', 'ini', 'conf', 'cfg', 'config', 'properties',
  'xml', 'json', 'yaml', 'yml', 'toml', 'csv', 'tsv', 'env', 'service', 'rules',
  'php', 'js', 'mjs', 'cjs', 'ts', 'jsx', 'tsx', 'css', 'scss', 'less', 'html', 'htm',
  'sh', 'bash', 'zsh', 'py', 'go', 'rs', 'java', 'c', 'h', 'cpp', 'hpp', 'cs', 'sql',
  'bat', 'cmd', 'ps1', 'vim', 'lua', 'rb', 'pl', 'asp', 'aspx', 'jsp', 'vue',
])
const TEXT_NAMES = new Set([
  'readme', 'license', 'changelog', 'makefile', 'dockerfile', 'procfile',
  '.env', '.gitignore', '.gitattributes', '.htaccess', '.editorconfig', '.npmrc',
])

// 可选编码与 Agent 侧的标签一一对应
const ENCODINGS = [
  'UTF-8', 'UTF-8 BOM', 'UTF-16LE', 'UTF-16BE',
  'GB18030', 'GBK', 'Big5', 'Shift_JIS', 'Windows-1252', 'ISO-8859-1',
]

function baseName(p) {
  const s = String(p || '')
  const cut = Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\'))
  return cut >= 0 ? s.slice(cut + 1) : s
}

function isTextFile(name) {
  const lower = String(name || '').toLowerCase()
  if (TEXT_NAMES.has(lower)) return true
  const i = lower.lastIndexOf('.')
  if (i <= 0) return false
  return TEXT_EXTS.has(lower.slice(i + 1))
}

function fmtSize(n) {
  if (typeof n !== 'number' || n < 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`
}

// 列表里的修改时间只需要"能认出是哪一条"，精确到分钟即可；
// 完整时间戳放在 title 里，避免这一列把行高与列宽都撑开。
function fmtTime(ts) {
  if (!ts) return '—'
  try {
    const d = new Date(ts * 1000)
    const md = `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`
    const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
    return `${md} ${hm}`
  } catch { return '—' }
}

function fullTime(ts) {
  if (!ts) return ''
  try { return new Date(ts * 1000).toLocaleString() } catch { return '' }
}

function fmtCountdown(sec) {
  const s = Math.max(0, Math.floor(sec || 0))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

// 去掉路径最后一段（Windows 与 Unix 通用），用于"是否已在当前目录"的判断。
function dirOf(p) {
  return String(p || '').replace(/[\\/][^\\/]*$/, '')
}

function sameDir(p, dir) {
  const norm = (s) => String(s || '').replace(/[\\/]+$/, '').toLowerCase()
  return norm(dirOf(p)) === norm(dir)
}

// 当前路径落在哪个卷上：取"路径前缀匹配且最长"的挂载点（/ 与 C:\ 都适用）。
function pickVolume(volumes, path) {
  if (!volumes?.length || !path) return null
  const norm = (s) => String(s || '').replace(/\\/g, '/').replace(/\/+$/, '').toLowerCase()
  const p = norm(path)
  let best = null
  let bestLen = -1
  for (const v of volumes) {
    const vp = norm(v.path)
    if (p === vp || p.startsWith(vp + '/')) {
      if (vp.length > bestLen) { best = v; bestLen = vp.length }
    }
  }
  return best
}

export default function Files({ agents }) {
  const [params] = useSearchParams()
  const wanted = params.get('agent') || ''
  const onlineHosts = (agents || []).filter((a) => a.online)

  const [host, setHost] = useState(wanted)
  const [session, setSession] = useState(null)
  const [path, setPath] = useState('')
  const [pathInput, setPathInput] = useState('')
  const [listing, setListing] = useState(null)
  const [loading, setLoading] = useState(false)
  const [notice, setNotice] = useState(null) // {ok, msg}
  const [idleLeft, setIdleLeft] = useState(0)
  const [uploading, setUploading] = useState(null) // {name, pct}
  const [selected, setSelected] = useState(() => new Set())
  const [clip, setClip] = useState(null) // {mode:'copy'|'cut', items, from}
  const [clipOpen, setClipOpen] = useState(false)
  const [clipPage, setClipPage] = useState(1)
  const [busy, setBusy] = useState('') // 批量操作的进行中文案
  const [diskOpen, setDiskOpen] = useState(false)
  const [editor, setEditor] = useState(null) // 文本编辑器：{path,name,content,encoding,detected,...}
  const [editLimitMB, setEditLimitMB] = useState(10)
  // 应用内弹窗：不用 window.prompt/confirm —— Electron/内嵌 WebView 会禁用它们
  const [dialog, setDialog] = useState(null)
  const fileInput = useRef(null)
  const sessionRef = useRef(null)

  useEffect(() => { sessionRef.current = session }, [session])

  // 在线编辑上限来自「系统配置」，页面加载时取一次；取不到就按默认 10 MiB 提示。
  useEffect(() => {
    Api.systemConfig()
      .then((d) => { if (d?.file_edit_max_mb) setEditLimitMB(d.file_edit_max_mb) })
      .catch(() => {})
  }, [])

  // 离开页面时主动断开（服务端还有空闲兜底，这里只是即时释放）
  useEffect(() => () => {
    const s = sessionRef.current
    if (s) Api.fileSessionClose(s.session_id).catch(() => {})
  }, [])

  const endSession = useCallback((msg) => {
    setSession(null)
    setListing(null)
    setPath('')
    setIdleLeft(0)
    setSelected(new Set())
    setClip(null)
    setClipOpen(false)
    setEditor(null)
    if (msg) setNotice({ ok: false, msg })
  }, [])

  const load = useCallback(async (s, p) => {
    if (!s) return
    setLoading(true)
    try {
      const d = await Api.fileList(s.session_id, p)
      if (!d.ok) {
        if (String(d.msg || '').includes('会话')) endSession(d.msg)
        else setNotice({ ok: false, msg: d.msg || '读取目录失败' })
        return
      }
      setListing(d)
      setPath(d.path || p)
      setPathInput(d.path || p)
      setIdleLeft(s.idle_seconds || 0)
      setSelected(new Set())
    } catch (e) {
      setNotice({ ok: false, msg: '读取目录失败：' + (e.response?.data?.msg || e.message) })
    } finally {
      setLoading(false)
    }
  }, [endSession])

  const connect = async () => {
    if (!host) return
    setLoading(true)
    setNotice(null)
    try {
      const d = await Api.fileSession(host)
      if (!d.ok) {
        setNotice({ ok: false, msg: d.msg || '建立会话失败' })
        return
      }
      const s = {
        session_id: d.session_id, agent_id: d.agent_id,
        roots: d.roots || [], root: d.root,
        idle_seconds: d.idle_seconds || 1200,
      }
      setSession(s)
      setPath(s.root)
      setPathInput(s.root)
      setIdleLeft(s.idle_seconds)
      await load(s, s.root)
    } catch (e) {
      setNotice({ ok: false, msg: '建立会话失败：' + (e.response?.data?.msg || e.message) })
    } finally {
      setLoading(false)
    }
  }

  const disconnect = async () => {
    const s = sessionRef.current
    if (s) {
      try { await Api.fileSessionClose(s.session_id) } catch { /* 服务端可能已超时 */ }
    }
    endSession('已断开文件会话')
  }

  // 空闲倒计时 + 每 30 秒同步一次服务端剩余时间。
  // 注意：状态查询**不会**刷新服务端的活跃时间，所以这里看到的就是真实剩余。
  useEffect(() => {
    if (!session) return
    const tick = setInterval(() => setIdleLeft((v) => Math.max(0, v - 1)), 1000)
    const poll = setInterval(async () => {
      try {
        const d = await Api.fileSessionStatus(session.session_id)
        if (!d.ok) { endSession(d.msg || '文件会话已断开'); return }
        setIdleLeft(d.idle_left)
      } catch { /* 网络抖动：让本地倒计时继续，下一轮再同步 */ }
    }, 30000)
    return () => { clearInterval(tick); clearInterval(poll) }
  }, [session, endSession])

  useEffect(() => {
    if (session && idleLeft === 0) {
      endSession('因无操作 20 分钟已自动断开，请重新连接')
    }
  }, [idleLeft, session, endSession])

  const enterDir = (p) => { if (!loading) load(session, p) }

  const joinPath = (dir, name) => {
    const d = String(dir || '')
    const sep = d.includes('\\') ? '\\' : '/'
    if (!d) return name
    return d.endsWith(sep) ? d + name : d + sep + name
  }

  const runOp = async (op, payload, okMsg) => {
    if (!session) return false
    setLoading(true)
    setNotice(null)
    try {
      const d = await Api.fileOp(session.session_id, op, payload)
      if (!d.ok) {
        setNotice({ ok: false, msg: d.msg || '操作失败' })
        return false
      }
      setNotice({ ok: true, msg: okMsg || d.msg || '操作完成' })
      await load(session, path)
      return true
    } catch (e) {
      const msg = e.response?.data?.msg || e.message
      setNotice({ ok: false, msg: '操作失败：' + msg })
      return false
    } finally {
      setLoading(false)
    }
  }

  // 批量操作：逐条下发（Agent 的每个操作都是原子的），最后汇总成功/失败数量。
  const runBatch = async (label, items, fn) => {
    setBusy(label)
    setNotice(null)
    let ok = 0
    let fail = 0
    let firstErr = ''
    for (const it of items) {
      try {
        const d = await fn(it)
        if (d.ok) ok++
        else { fail++; firstErr = firstErr || d.msg }
      } catch (e) {
        fail++
        firstErr = firstErr || (e.response?.data?.msg || e.message)
      }
      setBusy(`${label}（${ok + fail}/${items.length}）`)
    }
    setBusy('')
    setLoading(true)
    try { await load(session, path) } finally { setLoading(false) }
    return { ok, fail, firstErr }
  }

  const createEntry = (isDir) => {
    const label = isDir ? '文件夹' : '文件'
    setDialog({
      title: `新建${label}`,
      label: '名称（不含路径）',
      value: '',
      needsInput: true,
      onOk: async (v) => {
        if (v.includes('/') || v.includes('\\')) {
          setNotice({ ok: false, msg: '名称不能包含路径分隔符' })
          return
        }
        await runOp('create', { path: joinPath(path, v), is_dir: isDir }, `${label}已创建`)
      },
    })
  }

  const renameEntry = (entry) => {
    setDialog({
      title: `重命名 ${entry.name}`,
      label: '新名字（不含路径）',
      value: entry.name,
      needsInput: true,
      onOk: async (v) => {
        if (v === entry.name) return
        if (v.includes('/') || v.includes('\\')) {
          setNotice({ ok: false, msg: '名字不能包含路径分隔符' })
          return
        }
        await runOp('rename', { path: entry.path, dest: joinPath(path, v) }, `已重命名为 ${v}`)
      },
    })
  }

  const removeEntry = (entry) => {
    const what = entry.dir ? '目录（将递归删除）' : '文件'
    setDialog({
      title: '确认删除',
      text: `即将删除${what}：${entry.path}。删除后不可恢复。`,
      danger: true,
      onOk: async () => { await runOp('delete', { path: entry.path }, '已删除') },
    })
  }

  // ---- 多选 ----
  const entries = listing?.entries || []
  const selectedEntries = entries.filter((e) => selected.has(e.path))
  const allSelected = entries.length > 0 && selected.size >= entries.length

  const toggleSel = (p) => setSelected((prev) => {
    const next = new Set(prev)
    if (next.has(p)) next.delete(p)
    else next.add(p)
    return next
  })

  const toggleAll = () => setSelected((prev) => (
    prev.size >= entries.length ? new Set() : new Set(entries.map((e) => e.path))
  ))

  // ---- 剪贴板 ----
  const putClipboard = (mode, list) => {
    const items = list.slice(0, CLIP_MAX).map((e) => ({
      path: e.path, name: e.name, dir: e.dir, size: e.size,
    }))
    setClip({ mode, items, from: path })
    setClipOpen(false)
    setClipPage(1)
    setSelected(new Set())
    const verb = mode === 'cut' ? '剪切' : '复制'
    const extra = list.length > CLIP_MAX ? `（超出上限，只取了前 ${CLIP_MAX} 项）` : ''
    setNotice({ ok: true, msg: `已${verb} ${items.length} 项${extra}，进入目标目录后点「粘贴」` })
  }

  const pasteClip = async () => {
    if (!clip || !session) return
    const todo = clip.items.filter((it) => !sameDir(it.path, path))
    const skipped = clip.items.length - todo.length
    if (!todo.length) {
      setNotice({ ok: false, msg: '这些条目已经在当前目录里，无需粘贴' })
      return
    }
    const existing = new Set(entries.map((e) => e.name.toLowerCase()))
    const conflicts = todo.filter((it) => existing.has(it.name.toLowerCase()))
    const run = async () => {
      const moving = clip.mode === 'cut'
      const r = await runBatch(moving ? '正在移动' : '正在复制', todo, (it) =>
        Api.fileOp(session.session_id, moving ? 'move' : 'copy', { path: it.path, dest: joinPath(path, it.name) }))
      if (moving) setClip(null)
      const parts = []
      if (r.ok) parts.push(`已粘贴 ${r.ok} 项`)
      if (r.fail) parts.push(`${r.fail} 项失败${r.firstErr ? '：' + r.firstErr : ''}`)
      if (skipped) parts.push(`${skipped} 项已在当前目录被跳过`)
      setNotice({ ok: r.fail === 0, msg: parts.join('；') })
    }
    if (conflicts.length) {
      const names = conflicts.slice(0, 5).map((c) => c.name).join('、')
      setDialog({
        title: '确认粘贴（有同名项）',
        danger: true,
        okText: '覆盖粘贴',
        text: `目标目录已有 ${conflicts.length} 项同名：${names}${conflicts.length > 5 ? ' …' : ''}。\n继续会覆盖同名文件（同名目录会合并），是否继续？`,
        onOk: run,
      })
      return
    }
    await run()
  }

  const removeSelected = () => {
    const list = selectedEntries
    if (!list.length) return
    const names = list.slice(0, 5).map((e) => e.name).join('、')
    setDialog({
      title: '确认删除',
      danger: true,
      text: `即将删除 ${list.length} 项：${names}${list.length > 5 ? ' …' : ''}。目录会递归删除，删除后不可恢复。`,
      onOk: async () => {
        const r = await runBatch('正在删除', list, (e) =>
          Api.fileOp(session.session_id, 'delete', { path: e.path }))
        setSelected(new Set())
        setNotice({
          ok: r.fail === 0,
          msg: `已删除 ${r.ok} 项${r.fail ? `；${r.fail} 项失败${r.firstErr ? '：' + r.firstErr : ''}` : ''}`,
        })
      },
    })
  }

  const submitDialog = async () => {
    const d = dialog
    if (!d) return
    if (d.needsInput) {
      const v = (d.value || '').trim()
      if (!v) { setNotice({ ok: false, msg: '请输入内容' }); return }
      setDialog(null)
      await d.onOk(v)
      return
    }
    setDialog(null)
    await d.onOk()
  }

  const gotoPath = () => {
    const p = (pathInput || '').trim()
    if (!p) return
    load(session, p)
  }

  // 根目录下拉只列会话可访问的根；当前路径落在哪个根下就显示哪个（进入子目录后
  // 不能把子目录也变成"根"，否则下拉里会冒出一串历史路径）。
  const roots = session?.roots || []
  const sep = (path || '').includes('\\') ? '\\' : '/'
  const activeRoot = roots.find((r) => path === r || path.startsWith(r.endsWith(sep) ? r : r + sep)) || roots[0] || ''

  const volumes = listing?.volumes || []
  const curVol = useMemo(() => pickVolume(volumes, path), [volumes, path])
  const usedPct = curVol && curVol.total > 0
    ? Math.min(100, Math.round(((curVol.total - curVol.free) * 100) / curVol.total))
    : 0

  const clipPageCount = clip ? Math.max(1, Math.ceil(clip.items.length / CLIP_PAGE)) : 1
  const clipRows = clip ? clip.items.slice((clipPage - 1) * CLIP_PAGE, clipPage * CLIP_PAGE) : []

  const doUpload = async (file) => {
    if (!file || !session) return
    setUploading({ name: file.name, pct: 0 })
    setNotice(null)
    try {
      const d = await Api.fileUpload(session.session_id, path, file.name, file, (ev) => {
        if (ev.total) setUploading({ name: file.name, pct: Math.min(100, Math.round((ev.loaded * 100) / ev.total)) })
      })
      if (d.ok) {
        setNotice({ ok: true, msg: `已上传 ${file.name}（${fmtSize(d.size)}）` })
        await load(session, path)
      } else {
        setNotice({ ok: false, msg: d.msg || '上传失败' })
      }
    } catch (e) {
      const msg = e.response?.data?.msg || e.message
      setNotice({ ok: false, msg: '上传失败：' + msg })
    } finally {
      setUploading(null)
    }
  }

  // 同名覆盖是不可逆的，先确认；确认框用应用内 Modal（内嵌 WebView 禁用了 window.confirm）。
  const pickUpload = (file) => {
    if (!file || !session) return
    if (file.size > 64 * 1024 * 1024) {
      setNotice({ ok: false, msg: '单文件上限 64 MiB' })
      return
    }
    setDialog({
      title: '确认上传',
      text: `即将上传 ${file.name}（${fmtSize(file.size)}）到：${path}\n同名文件会被覆盖。`,
      onOk: async () => { await doUpload(file) },
    })
  }

  const download = async (entry) => {
    setNotice(null)
    try {
      const res = await Api.fileDownload(session.session_id, entry.path)
      const blob = res.data
      // 服务端错误是非 200 + JSON，但保险起见再判一次内容类型
      if (blob.type && blob.type.includes('application/json')) {
        const j = JSON.parse(await blob.text())
        setNotice({ ok: false, msg: j.msg || '下载失败' })
        return
      }
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = entry.name
      document.body.appendChild(a)
      a.click()
      a.remove()
      setTimeout(() => URL.revokeObjectURL(url), 5000)
      setNotice({ ok: true, msg: `已开始下载 ${entry.name}（${fmtSize(entry.size || 0)}）` })
    } catch (e) {
      // blob 响应里的 JSON 错误要解析出来，否则只会看到一句 "Request failed with status code 502"
      let msg = e.response?.data?.msg
      if (!msg && e.response?.data instanceof Blob) {
        try { msg = JSON.parse(await e.response.data.text()).msg } catch { /* ignore */ }
      }
      setNotice({ ok: false, msg: '下载失败：' + (msg || e.message) })
    }
  }

  // ---- 文本预览 / 编辑 ----
  const editLimitBytes = editLimitMB * 1024 * 1024

  // keepOnError：换编码重载失败时保留编辑窗口（否则用户刚敲的内容直接消失）。
  const loadText = async (p, enc, readOnly = false, keepOnError = false) => {
    if (!session) return
    setNotice(null)
    try {
      const d = await Api.fileText(session.session_id, p, enc)
      if (!d.ok) {
        if (keepOnError) setEditor((cur) => (cur ? { ...cur, saving: false, loading: false } : cur))
        else setEditor(null)
        setNotice({ ok: false, msg: d.msg || '打开失败' })
        return
      }
      setEditor({
        path: p, name: baseName(p), content: d.content || '',
        encoding: d.encoding || 'UTF-8', detected: d.detected || '',
        size: d.size || 0, dirty: false, readOnly, saving: false, loading: false,
      })
    } catch (e) {
      setEditor(null)
      setNotice({ ok: false, msg: '打开失败：' + (e.response?.data?.msg || e.message) })
    }
  }

  const openEditor = (entry) => {
    if ((entry.size || 0) > editLimitBytes) {
      setNotice({ ok: false, msg: `文件 ${fmtSize(entry.size)} 超过在线编辑上限 ${editLimitMB} MiB，请下载后本地打开` })
      return
    }
    setEditor({
      path: entry.path, name: entry.name, content: '', encoding: 'UTF-8', detected: '',
      size: entry.size || 0, dirty: false, readOnly: false, saving: false, loading: true,
    })
    loadText(entry.path, '')
  }

  // 换了编码要按新编码重读一遍：同一份字节用错编码解出来就是乱码。
  const reloadText = () => {
    if (!editor) return
    const go = () => loadText(editor.path, editor.encoding, editor.readOnly, true)
    if (editor.dirty) {
      setDialog({
        title: '重新载入',
        danger: true,
        okText: '放弃并重载',
        text: '按所选编码重新载入会丢弃当前未保存的修改。',
        onOk: async () => go(),
      })
      return
    }
    go()
  }

  const saveEditor = async () => {
    const cur = editor
    if (!cur || !session) return
    setEditor({ ...cur, saving: true })
    try {
      const d = await Api.fileTextSave(session.session_id, cur.path, cur.encoding, cur.content)
      if (!d.ok) {
        setEditor({ ...cur, saving: false })
        setNotice({ ok: false, msg: d.msg || '保存失败' })
        return
      }
      setEditor({ ...cur, saving: false, dirty: false, size: d.size || 0 })
      setNotice({ ok: true, msg: `已保存 ${cur.name}（${fmtSize(d.size || 0)}，${d.encoding || cur.encoding}）` })
      await load(session, path)
    } catch (e) {
      setEditor({ ...cur, saving: false })
      setNotice({ ok: false, msg: '保存失败：' + (e.response?.data?.msg || e.message) })
    }
  }

  const closeEditor = () => {
    if (!editor) return
    if (editor.dirty) {
      setDialog({
        title: '放弃修改',
        danger: true,
        okText: '放弃修改',
        text: `《${editor.name}》有未保存的修改，关闭后这些修改会丢失。`,
        onOk: async () => setEditor(null),
      })
      return
    }
    setEditor(null)
  }

  const idleMinutes = Math.round((session?.idle_seconds || 1200) / 60)

  return (
    <div className="fm-page">
      <div className="page-head fm-head">
        <div>
          <h2>文件管理</h2>
          <div className="sub">
            整机浏览与维护被管主机文件 · 新建 / 重命名 / 复制 / 剪切 / 粘贴 / 删除 / 上传 / 下载
            （以 Agent 服务账号权限执行）
          </div>
        </div>
        {session && (
          <span className={`badge ${idleLeft > 120 ? 'online' : 'idle'}`} title={`无操作 ${idleMinutes} 分钟自动断开`}>
            空闲断开 {fmtCountdown(idleLeft)}
          </span>
        )}
      </div>

      <div className="card fm-card">
        <div className="fm-bar">
          <div className="fm-field fm-host">
            <label>目标主机</label>
            <select
              value={host}
              onChange={(e) => setHost(e.target.value)}
              disabled={!!session}
            >
              <option value="">请选择在线主机...</option>
              {onlineHosts.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>{hostLabel(a)}</option>
              ))}
            </select>
          </div>
          {session && (
            <div className="fm-field fm-path">
              <label>路径（回车跳转）</label>
              <input
                value={pathInput}
                onChange={(e) => setPathInput(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter') gotoPath() }}
                placeholder="绝对路径，如 /var/log 或 C:\Windows"
                spellCheck={false}
              />
            </div>
          )}
          {session && roots.length > 0 && (
            <div className="fm-field fm-root">
              <label>根目录</label>
              <select
                value={activeRoot}
                onChange={(e) => enterDir(e.target.value)}
                disabled={loading}
                title="切换会话可访问的根目录"
              >
                {roots.map((r) => <option key={r} value={r}>{r}</option>)}
              </select>
            </div>
          )}
          <div className="fm-actions">
            {!session ? (
              <button className="primary" onClick={connect} disabled={!host || loading}>
                {loading ? '连接中…' : '连接文件管理'}
              </button>
            ) : (
              <>
                <button className="secondary" onClick={() => fileInput.current?.click()} disabled={!!uploading || !!busy}>
                  {uploading ? `上传中 ${uploading.pct}%` : '上传文件'}
                </button>
                <button className="secondary" onClick={() => createEntry(false)} disabled={loading || !!busy}>新建文件</button>
                <button className="secondary" onClick={() => createEntry(true)} disabled={loading || !!busy}>新建文件夹</button>
                <button className="secondary" onClick={() => load(session, path)} disabled={loading || !!busy}>刷新</button>
                <button className="secondary danger" onClick={disconnect}>断开</button>
              </>
            )}
            <input
              ref={fileInput}
              type="file"
              style={{ display: 'none' }}
              onChange={(e) => { const f = e.target.files?.[0]; e.target.value = ''; if (f) pickUpload(f) }}
            />
          </div>
        </div>

        {session && (
          <div className="fm-meta">
            <span title="会话所属主机">{hostLabel((agents || []).find((a) => a.agent_id === session.agent_id) || { agent_id: session.agent_id })}</span>
            <code title={path}>{path || '—'}</code>
            <span className="fm-meta-hint">{(listing?.entries || []).length} 项</span>
            <button className="linklike fm-selall" onClick={toggleAll} disabled={!entries.length}>
              {allSelected ? '取消全选' : '全选'}
            </button>
            {curVol && (
              <button className="fm-disk" onClick={() => setDiskOpen(true)} title="查看全部分区">
                <span className="fm-disk-vol">{curVol.name}</span>
                <span className="fm-disk-fs">{curVol.fs_type || '未知格式'}</span>
                <span className="fm-disk-bar"><i className={usedPct >= 90 ? 'danger' : usedPct >= 75 ? 'warn' : ''} style={{ width: usedPct + '%' }} /></span>
                <span className="fm-disk-text">已用 {usedPct}% · 剩余 {fmtSize(curVol.free)} / {fmtSize(curVol.total)}</span>
              </button>
            )}
          </div>
        )}

        {selected.size > 0 && (
          <div className="fm-selbar">
            <span>已选 <b>{selected.size}</b> 项</span>
            <button className="secondary btn-sm" disabled={!!busy} onClick={() => putClipboard('copy', selectedEntries)}>复制</button>
            <button className="secondary btn-sm" disabled={!!busy} onClick={() => putClipboard('cut', selectedEntries)}>剪切</button>
            <button className="secondary danger btn-sm" disabled={!!busy} onClick={removeSelected}>删除</button>
            <button className="linklike" onClick={() => setSelected(new Set())}>取消选择</button>
          </div>
        )}

        {clip && (
          <div className="fm-clipbar">
            <span className="fm-clip-label">{clip.mode === 'cut' ? '剪贴板（剪切）' : '剪贴板（复制）'} · {clip.items.length} 项</span>
            <span className="fm-clip-from" title={clip.from}>来自 {clip.from}</span>
            <button className="primary btn-sm" onClick={pasteClip} disabled={loading || !!busy}>粘贴</button>
            <button
              className="secondary btn-sm"
              onClick={() => setClipOpen((v) => !v)}
              aria-expanded={clipOpen}
            >
              {clipOpen ? '收起列表' : `列表（${clip.items.length}）`}
            </button>
            <button className="linklike" onClick={() => { setClip(null); setClipOpen(false) }}>清空</button>
            {busy && <span className="fm-clip-busy">{busy}</span>}
          </div>
        )}

        {clip && clipOpen && (
          <div className="fm-cliplist">
            <table className="fm-mini">
              <thead>
                <tr><th>名称</th><th>原路径</th><th className="fm-col-size">大小</th></tr>
              </thead>
              <tbody>
                {clipRows.map((it) => (
                  <tr key={it.path}>
                    <td>{it.dir ? '📁' : '📄'} {it.name}</td>
                    <td title={it.path}>{dirOf(it.path)}</td>
                    <td className="fm-col-size">{it.dir ? '—' : fmtSize(it.size || 0)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="fm-clipfoot">
              <button className="secondary btn-sm" onClick={() => setClipPage((p) => Math.max(1, p - 1))} disabled={clipPage <= 1}>上一页</button>
              <span>第 {clipPage} / {clipPageCount} 页 · 共 {clip.items.length} 项（上限 {CLIP_MAX}）</span>
              <button className="secondary btn-sm" onClick={() => setClipPage((p) => Math.min(clipPageCount, p + 1))} disabled={clipPage >= clipPageCount}>下一页</button>
            </div>
          </div>
        )}

        {notice && <div className={`notice fm-notice ${notice.ok ? 'ok' : 'err'}`}>{notice.msg}</div>}

        {session ? (
          <div className="table-scroll">
            <table className="table-stack fm-table">
              <thead>
                <tr>
                  <th className="fm-col-check" aria-label="选择"></th>
                  <th>名称</th>
                  <th className="fm-col-size">大小</th>
                  <th className="fm-col-time">修改时间</th>
                  <th className="fm-col-ops">操作</th>
                </tr>
              </thead>
              <tbody>
                {listing?.parent && (
                  <tr className="fm-up">
                    <td colSpan="5" data-label="名称">
                      <button className="linklike" onClick={() => enterDir(listing.parent)}>⬆ 上一级</button>
                    </td>
                  </tr>
                )}
                {entries.map((e) => (
                  <tr key={e.path} className={selected.has(e.path) ? 'fm-row-sel' : ''}>
                    <td className="fm-col-check" data-label="选择">
                      <input
                        type="checkbox"
                        checked={selected.has(e.path)}
                        onChange={() => toggleSel(e.path)}
                        aria-label={`选择 ${e.name}`}
                      />
                    </td>
                    <td data-label="名称" title={e.path}>
                      {e.dir ? (
                        <button className="linklike" onClick={() => enterDir(e.path)}>📁 {e.name}</button>
                      ) : (
                        <span className="fm-name">{e.symlink ? '🔗' : '📄'} {e.name}</span>
                      )}
                    </td>
                    <td data-label="大小" className={e.dir ? 'fm-col-size fm-na' : 'fm-col-size'}>{e.dir ? '—' : fmtSize(e.size || 0)}</td>
                    <td data-label="修改时间" className="fm-col-time" title={fullTime(e.mod_time)}>{fmtTime(e.mod_time)}</td>
                    <td data-label="操作">
                      <div className="row-actions">
                        {!e.dir && (
                          <button className="secondary btn-sm" onClick={() => download(e)}>下载</button>
                        )}
                        {!e.dir && isTextFile(e.name) && (
                          <button
                            className="secondary btn-sm"
                            onClick={() => openEditor(e)}
                            disabled={(e.size || 0) > editLimitBytes}
                            title={(e.size || 0) > editLimitBytes
                              ? `超过在线编辑上限（${editLimitMB} MiB），请下载后本地打开`
                              : '在线预览 / 编辑'}
                          >
                            编辑
                          </button>
                        )}
                        <button className="secondary btn-sm" onClick={() => renameEntry(e)}>重命名</button>
                        <button className="secondary danger btn-sm" onClick={() => removeEntry(e)}>删除</button>
                      </div>
                    </td>
                  </tr>
                ))}
                {loading && <tr><td colSpan="5" className="table-empty">加载中…</td></tr>}
                {!loading && listing && entries.length === 0 && (
                  <tr><td colSpan="5" className="table-empty">目录为空</td></tr>
                )}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="fm-empty">选择一台在线主机后点击「连接文件管理」，即可浏览该主机的整机文件。</div>
        )}
        {listing?.truncated && (
          <div className="fm-truncated">目录条目过多，仅显示前 1000 项（目录优先、按名称排序）。</div>
        )}
      </div>

      <details className="fm-safety">
        <summary>安全说明</summary>
        <div className="desc">
          <b>本页以 Agent 服务账号（root / SYSTEM）身份访问整台主机</b>，不再限制在
          <code>-file-roots</code> 沙箱内。删除会递归删除目录且不可恢复，执行前请核对完整路径；
          重命名不会覆盖已存在的目标，粘贴遇到同名文件会先确认。
          所有新建/重命名/复制/移动/删除/上传/下载都写入操作审计。
          会话无操作 {idleMinutes} 分钟自动断开（不因使用时长被踢），离开页面也会主动关闭；
          单文件上传/下载上限 64 MiB（更大的文件请用「运维操作 → 文件分发」或命令行）。
        </div>
      </details>

      {diskOpen && (
        <Modal title="磁盘与分区" onClose={() => setDiskOpen(false)}
          footer={<button className="secondary" onClick={() => setDiskOpen(false)}>关闭</button>}>
          <table className="fm-mini">
            <thead>
              <tr>
                <th>分区</th><th>文件系统</th><th>已用 / 总量</th><th>可用</th><th className="fm-disk-col">使用率</th>
              </tr>
            </thead>
            <tbody>
              {volumes.map((v) => {
                const pct = v.total > 0 ? Math.min(100, Math.round(((v.total - v.free) * 100) / v.total)) : 0
                return (
                  <tr key={v.path} className={curVol && curVol.path === v.path ? 'fm-row-sel' : ''}>
                    <td title={v.path}>{v.name}</td>
                    <td>{v.fs_type || '—'}</td>
                    <td>{fmtSize(v.total - v.free)} / {fmtSize(v.total)}</td>
                    <td>{fmtSize(v.free)}</td>
                    <td className="fm-disk-col">
                      <span className="fm-disk-bar"><i className={pct >= 90 ? 'danger' : pct >= 75 ? 'warn' : ''} style={{ width: pct + '%' }} /></span>
                      <span className="fm-disk-pct">{pct}%</span>
                    </td>
                  </tr>
                )
              })}
              {volumes.length === 0 && <tr><td colSpan="5" className="table-empty">该主机未上报分区信息</td></tr>}
            </tbody>
          </table>
        </Modal>
      )}

      {editor && (
        <Modal
          wide
          title={`编辑 ${editor.name}`}
          subtitle={editor.path}
          onClose={closeEditor}
          footer={<>
            <button className="secondary" onClick={closeEditor}>关闭</button>
            <button
              className="primary"
              onClick={saveEditor}
              disabled={editor.saving || editor.loading || editor.readOnly || !editor.dirty}
            >
              {editor.saving ? '保存中…' : '保存'}
            </button>
          </>}
        >
          <div className="fm-editor-bar">
            <label className="fm-editor-enc">
              编码
              <select
                value={editor.encoding}
                onChange={(e) => setEditor({ ...editor, encoding: e.target.value })}
                disabled={editor.loading || editor.saving}
              >
                {ENCODINGS.map((enc) => <option key={enc} value={enc}>{enc}</option>)}
                {!ENCODINGS.includes(editor.encoding) && <option value={editor.encoding}>{editor.encoding}</option>}
              </select>
            </label>
            <button className="secondary btn-sm" onClick={reloadText} disabled={editor.loading || editor.saving}>
              按此编码重新载入
            </button>
            <label className="fm-editor-ro">
              <input
                type="checkbox"
                checked={editor.readOnly}
                onChange={(e) => setEditor({ ...editor, readOnly: e.target.checked })}
              />
              只读
            </label>
            <span className="cell-sub">
              {editor.loading
                ? '载入中…'
                : `${fmtSize(editor.size)}${editor.detected ? ` · 检测编码 ${editor.detected}` : ''}${editor.dirty ? ' · 有未保存修改' : ''}`}
            </span>
          </div>
          {editor.loading ? (
            <div className="fm-editor-loading">载入中…</div>
          ) : (
            <textarea
              className="fm-editor"
              value={editor.content}
              readOnly={editor.readOnly}
              onChange={(e) => setEditor({ ...editor, content: e.target.value, dirty: true })}
              spellCheck={false}
              wrap="off"
            />
          )}
        </Modal>
      )}

      {dialog && (
        <Modal
          title={dialog.title}
          onClose={() => setDialog(null)}
          footer={<>
            <button className="secondary" onClick={() => setDialog(null)}>取消</button>
            <button className={dialog.danger ? 'primary danger' : 'primary'} onClick={submitDialog}>
              {dialog.okText || (dialog.danger ? '删除' : '确定')}
            </button>
          </>}
        >
          {dialog.text && <div className="desc" style={{ whiteSpace: 'pre-wrap' }}>{dialog.text}</div>}
          {dialog.needsInput && (
            <div className="form-row">
              <label>{dialog.label}</label>
              <input
                autoFocus
                value={dialog.value}
                onChange={(e) => setDialog({ ...dialog, value: e.target.value })}
                onKeyDown={(e) => { if (e.key === 'Enter') submitDialog() }}
              />
            </div>
          )}
        </Modal>
      )}
    </div>
  )
}
