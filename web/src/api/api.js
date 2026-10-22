// API 客户端：封装对 MoCenter 服务端 REST API 的请求。
import axios from 'axios'

const api = axios.create({
  baseURL: '/api',
  timeout: 10000,
})

// 请求拦截器：附加 Token
api.interceptors.request.use((config) => {
  const token = localStorage.getItem('mc_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// 响应拦截器：401 时自动清除 Token 并跳转登录页
api.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      localStorage.removeItem('mc_token')
      // 不在登录页时跳转；登录页自身的 401 由页面逻辑处理。
      // 注意用 /admin/login —— 根级 /login 只作为兼容别名重定向过来，
      // 在它上面判断会漏掉真实地址（/admin/login）。
      if (!window.location.pathname.startsWith('/admin/login')) {
        const current = window.location.pathname + window.location.search
        window.location.href = `/admin/login?redirect=${encodeURIComponent(current)}`
      }
    }
    return Promise.reject(error)
  }
)

export const Api = {
  // Agent 接入信息（服务端地址/身份指纹/共享密钥指纹），用于生成"一条命令"接入新主机
  async serverInfo() {
    const { data } = await api.get('/server-info')
    return data
  },
  // 一次性注册令牌：列出（不含明文）+ 证书 TOFU 绑定 + 共享密钥是否仍可用
  async enrollTokens() {
    const { data } = await api.get('/enroll/tokens')
    return data
  },
  // 签发一枚绑定某台主机的注册令牌；明文只在本次响应里出现
  async issueEnrollToken(payload) {
    const { data } = await api.post('/enroll/tokens', payload)
    return data
  },
  // 吊销令牌：传 id 单条吊销，传 agent_id 清掉该机全部未使用令牌
  async revokeEnrollToken(payload) {
    const { data } = await api.post('/enroll/token/revoke', payload)
    return data
  },
  // 清除某台主机的证书公钥绑定（误绑定/迁移旧数据时的显式授权口）
  async resetEnrollBinding(agentId) {
    const { data } = await api.post('/enroll/cert/reset', { agent_id: agentId })
    return data
  },
  // 永久删除一枚"已注销主机"的令牌记录（主机仍在册 / 从未接入过时服务端会拒绝）
  async deleteEnrollToken(id) {
    const { data } = await api.post('/enroll/token/delete', { id })
    return data
  },
  // 轮换部署级共享注册密钥：新明文只在这次响应里出现一次
  async rotateSharedKey() {
    const { data } = await api.post('/enroll/shared-key/rotate', { confirm: true })
    return data
  },
  // 会话交接：修改监听端口后在新 origin 恢复登录（一次性、60 秒有效）
  async createHandoff() {
    const { data } = await api.post('/session/handoff')
    return data
  },
  async consumeHandoff(code) {
    const { data } = await api.post('/session/handoff/consume', { code })
    return data
  },
  // 系统配置：会话保持时长 + Agent/后台监听地址
  async systemConfig() {
    const { data } = await api.get('/system/config')
    return data
  },
  async setSessionTTL(ttl) {
    const { data } = await api.post('/system/session', { ttl })
    return data
  },
  async setAgentListen(payload) {
    const { data } = await api.post('/system/agent-listen', payload)
    return data
  },
  async setHTTPListen(payload) {
    const { data } = await api.post('/system/http-listen', payload)
    return data
  },
  // 远程设置 Agent 日志级别：{agent_id, level} 或 {all:true, level}
  async setAgentLogLevel(payload) {
    const { data } = await api.post('/agents/log-level', payload)
    return data
  },
  // 获取全部主机（含离线）
  async agents() {
    const { data } = await api.get('/agents')
    return data
  },
  // 删除一台离线主机的登记与历史数据（不会卸载目标机上的 Agent）
  async deleteAgent(agentId) {
    const { data } = await api.post('/agents/delete', { agent_id: agentId })
    return data
  },
  // --- 邀请远程管理：控制台侧创建/列举/撤销分享链接 ---
  // 链接 token 明文只在创建响应里出现一次，列表接口只返回元数据。
  async listShares(agentId) {
    const { data } = await api.get('/shares', { params: { agent: agentId } })
    return data
  },
  async createShare(payload) {
    const { data } = await api.post('/shares', payload)
    return data
  },
  async revokeShare(id) {
    const { data } = await api.post('/shares/revoke', { id })
    return data
  },
  // 获取主机历史探针趋势；max 控制返回采样点上限（服务端按需降采样）
  async history(agent, since, max) {
    const { data } = await api.get('/agents/history', { params: { agent, since, max } })
    return data
  },
  // 下发命令
  async command(req) {
    const { data } = await api.post('/command', req)
    return data
  },
  // 审计日志（服务端分页）：返回 { items, total }
  async audits(params = {}) {
    const { data } = await api.get('/audits', { params })
    // 兼容旧版服务端直接返回数组的情形
    if (Array.isArray(data)) {
      return { items: data, total: data.length }
    }
    return { items: data?.items || [], total: data?.total || 0 }
  },
  // 导出审计日志为 CSV（当前筛选条件下的当前页）
  async auditsCsv(params = {}) {
    const { data } = await api.get('/audits', { params: { ...params, format: 'csv' }, responseType: 'blob', timeout: 30000 })
    return data
  },
  // 批量命令
  async batchCommand(req) {
    const { data } = await api.post('/command/batch', req)
    return data
  },
  // 端口映射规则
  async portmapRules() {
    const { data } = await api.get('/portmap/rules')
    return data
  },
  // 端口映射操作
  async portmapOp(op) {
    const { data } = await api.post('/portmap/op', op)
    return data
  },
  // 远程升级
  async upgrade(req) {
    const { data } = await api.post('/upgrade', req)
    return data
  },
  // 健康检查
  async health() {
    const { data } = await api.get('/health')
    return data
  },
  // 用户名+密码（+OTP 可选）登录，成功返回 { token }
  async login({ username, password, otp }) {
    const { data } = await api.post('/login', { username, password, otp })
    return data
  },
  // 登出：吊销当前会话 Token
  async logout() {
    const { data } = await api.post('/logout')
    return data
  },
  // 修改用户密码
  async changePassword(payload) {
    const { data } = await api.post('/password', payload)
    return data
  },
  // 生成 OTP 绑定密钥与 otpauth URI（返回 { secret, uri, enabled }）
  // 服务端以当前会话身份为准，无需再传 username；OTP 已启用时必须带当前密码，
  // 否则任何持有会话 Token 的人都能把验证器换掉。
  async otpSetup(payload = {}) {
    const { data } = await api.post('/otp/setup', payload)
    return data
  },
  // 校验动态验证码并启用 OTP
  async otpEnable(payload) {
    const { data } = await api.post('/otp/enable', payload)
    return data
  },
  // 关闭 OTP（需密码，启用中还需当前验证码）
  async otpDisable(payload) {
    const { data } = await api.post('/otp/disable', payload)
    return data
  },
  // 断开远程终端连接，结束相关进程
  async commandClose(payload) {
    const { data } = await api.post('/command/close', payload)
    return data
  },
  // 探测目标主机 VNC 是否可连
  async vncProbe(agent, port) {
    const { data } = await api.get('/vnc/probe', { params: { agent, port } })
    return data
  },
  // 一键下发 VNC 安装/启动指令（安装可能需下载+静默安装，给 120s 超时）
  async vncInstall(payload) {
    const { data } = await api.post('/vnc/install', payload, { timeout: 120000 })
    return data
  },
  // 查询最近一次 VNC 安装/启动结果（不含口令；has_password 表示服务端有口令可取）
  async vncStatus(agent) {
    const { data } = await api.get('/vnc/status', { params: { agent } })
    return data
  },
  // 按需取回某主机最近一次安装生成的 VNC 口令；服务端每次都留审计
  async vncPassword(agent) {
    const { data } = await api.post('/vnc/password', { agent_id: agent })
    return data
  },
  // 设置/更新主机备注
  async setRemark(agentId, remark) {
    const { data } = await api.post('/agents/remark', { agent_id: agentId, remark })
    return data
  },

  // --- 公开展示配置（登录后可改；决定未登录访客能看什么） ---

  // 读取公开展示配置与字段元数据
  async siteConfig() {
    const { data } = await api.get('/site/config')
    return data
  },
  // 保存公开展示配置
  async saveSiteConfig(cfg) {
    const { data } = await api.post('/site/config', cfg)
    return data
  },
  // 恢复默认（公开页关闭）
  async resetSiteConfig() {
    const { data } = await api.post('/site/config/reset')
    return data
  },
  // 可挑选的公开主机（最小字段列表）
  async siteHosts() {
    const { data } = await api.get('/site/hosts')
    return data
  },

  // --- 延迟监测（ICMP/TCP/HTTP） ---

  // 主机延迟历史曲线：返回 { ok, agent, targets, series:[{target,kind,points:[{ts,ms,ok}]}] }
  // 失败的探测点也在 points 里（ok=false），前端画断点而不是丢弃
  async latency(agent, range, target) {
    const { data } = await api.get('/latency', { params: { agent, range, target } })
    return data
  },
  // 读取某主机的延迟探测目标
  async latencyTargets(agent) {
    const { data } = await api.get('/latency/targets', { params: { agent } })
    return data
  },
  // 下发延迟探测目标（形如 icmp:1.1.1.1 / tcp:1.1.1.1:443 / http:https://x）
  async saveLatencyTargets(agentId, targets) {
    const { data } = await api.post('/latency/targets', { agent_id: agentId, targets })
    return data
  },

  // --- 版本与升级 ---

  // 服务端版本 + 各 Agent 版本与升级判定（含离线 GeoIP 库状态）
  async version() {
    const { data } = await api.get('/version')
    return data
  },

  // --- 备份 / 恢复 ---

  // 列出已有备份包（含包内清单摘要）
  async backups() {
    const { data } = await api.get('/backup')
    return data
  },
  // 创建备份；withSecrets=true 时把 agent.key 这类凭据文件一起打包（默认不含）
  async createBackup(withSecrets = false) {
    const { data } = await api.post('/backup', { include_secrets: !!withSecrets }, { timeout: 300000 })
    return data
  },
  // 查看备份包清单（恢复前确认）
  async inspectBackup(file) {
    const { data } = await api.post('/backup', null, { params: { action: 'inspect', file } })
    return data
  },
  // 从备份恢复（不可逆，前端需二次确认）
  async restoreBackup(file) {
    const { data } = await api.post('/backup', null, { params: { action: 'restore', file }, timeout: 300000 })
    return data
  },
  // 删除备份包
  async deleteBackup(file) {
    const { data } = await api.delete('/backup', { params: { file } })
    return data
  },
  // 下载备份包（走浏览器原生下载，带 Authorization 头）
  async downloadBackup(file) {
    const { data } = await api.get('/backup', { params: { download: file }, responseType: 'blob', timeout: 300000 })
    return data
  },

  // --- 一键部署 Agent ---

  // 可下发的 Agent 安装包
  async deployPackages() {
    const { data } = await api.get('/agents/deploy')
    return data
  },
  // 执行部署；probe_only=true 时仅做连通性与环境探测，不做任何改动
  async deployAgent(payload) {
    const { data } = await api.post('/agents/deploy', payload, { timeout: 180000 })
    return data
  },
}

// 公开站点 API（**免登录**）：
// 不带 Authorization 头，绝不调用带 token 的实例，避免把会话凭据带到匿名页面。
const publicApi = axios.create({ baseURL: '/api/public', timeout: 10000 })

// 品牌文案用的裸实例：与 publicApi 一样**绝不带 Authorization**（登录前调用，
// 带了就是把会话凭据往匿名页面上送），但它的 baseURL 是 /api ——
// /api/branding 不属于公开页功能，也就不该跟着公开页开关一起消失。
const brandingApi = axios.create({ baseURL: '/api', timeout: 10000 })

export const PublicApi = {
  // 公开页是否已启用（只返回布尔值，供登录页给出入口）。
  // 关掉时服务端直接 404，走 catch 返回 false —— 与"没连上"同样处理。
  async enabled() {
    try {
      const { data } = await publicApi.get('/enabled')
      return !!(data && data.enabled)
    } catch {
      return false
    }
  },
  // 站点品牌文案（标题 / 页脚）。它在**独立的** /api/branding 上，
  // 不跟公开页开关绑在一起：登录页在登录前就要显示正确的标签页标题，
  // 而那时候公开页可能压根没启用。
  async branding() {
    try {
      const { data } = await brandingApi.get('/branding')
      return {
        site_title: data?.site_title || '',
        site_footer: data?.site_footer || '',
      }
    } catch {
      return { site_title: '', site_footer: '' }
    }
  },
  // 公开状态：配置 + 字段元数据 + 节点列表 + 汇总
  async state() {
    const { data } = await publicApi.get('/state')
    return data
  },
  // 节点历史趋势（node 为派生节点编号）
  async samples(node, range) {
    const { data } = await publicApi.get('/samples', { params: { node, range } })
    return data
  },
}

// 公开站点 WebSocket：免登录实时推送（仅节点列表与汇总）。
// 与 MCSocket 分开实现：公开页不需要（也不应该）携带 Token 与命令通道。
export class PublicSocket {
  constructor({ onData, onStatus } = {}) {
    this.onData = onData
    this.onStatus = onStatus
    this.alive = true
    this.ws = null
    this.delay = 1000
  }

  connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    this.ws = new WebSocket(`${proto}://${location.host}/api/public/ws`)
    this.ws.onopen = () => {
      this.delay = 1000
      this.onStatus?.('connected')
    }
    this.ws.onmessage = (ev) => {
      try {
        this.onData?.(JSON.parse(ev.data))
      } catch { /* 忽略非 JSON 帧 */ }
    }
    this.ws.onclose = () => {
      this.onStatus?.('disconnected')
      if (!this.alive) return
      // 总开关关闭时端点返回 404（对外"不存在"）。这不是网络故障，
      // 无限重连只会在控制台刷屏并给服务端带来无意义请求 —— 停止重试，
      // 由页面的 HTTP 拉取结果（enabled=false / 404）驱动界面状态。
      if (this.gone) return
      // 指数退避重连：服务端被重启或网络抖动时不至于打满连接
      setTimeout(() => {
        this.delay = Math.min(this.delay * 2, 30000)
        this.connect()
      }, this.delay)
    }
    this.ws.onerror = () => {
      // 握手失败（未启用/服务未就绪）时不再重试，避免"未启用"部署下的控制台噪音
      this.gone = true
      this.ws && this.ws.close()
    }
  }

  close() {
    this.alive = false
    this.ws && this.ws.close()
  }
}

// 分享访客 API（免登录）：凭链接 token 走 X-Share-Token 头。
// 刻意不复用上面的 admin 实例 —— 它带 Authorization 且 401 时会跳登录页，
// 而访客既没有账号，也不该因为链接过期被带去控制台登录。
const shareClient = axios.create({ baseURL: '/api/share', timeout: 10000 })
const shareHeaders = (token) => ({ 'X-Share-Token': token || '' })

export const ShareApi = {
  // 分享范围与目标主机信息（主机名/在线状态/功能/有效期）
  async info(token) {
    const { data } = await shareClient.get('/info', { headers: shareHeaders(token) })
    return data
  },
  // 开一个交互终端（服务端只允许开给分享绑定的那台主机）
  async command(token, payload) {
    const { data } = await shareClient.post('/command', payload, { headers: shareHeaders(token) })
    return data
  },
  async commandClose(token, commandId) {
    const { data } = await shareClient.post('/command/close', { command_id: commandId }, { headers: shareHeaders(token) })
    return data
  },
  async rules(token) {
    const { data } = await shareClient.get('/portmap/rules', { headers: shareHeaders(token) })
    return data
  },
  async portmapOp(token, op) {
    const { data } = await shareClient.post('/portmap/op', op, { headers: shareHeaders(token) })
    return data
  },
  // 读取该主机当前生效的 VNC 口令（访客没有别的途径知道口令）
  async vncPassword(token) {
    const { data } = await shareClient.post('/vnc/password', null, { headers: shareHeaders(token) })
    return data
  },
}

// WebSocket 客户端：用于接收服务端实时推送（主机状态、命令输出等）。
export class MCSocket {
  constructor({ onAgentList, onCommandResp, onInteractive, onPortMap, onFileTransfer, onUpgrade, token }) {
    this.url = () => {
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      // Token 改走 Sec-WebSocket-Protocol 子协议头，不再拼在 URL 上：
      // URL 会进反向代理访问日志与浏览器历史，属于敏感信息泄露面。
      return `${proto}://${location.host}/ws`
    }
    this.protocols = ['mocenter', encodeURIComponent(token || '')]
    this.handlers = { onAgentList, onCommandResp, onInteractive, onPortMap, onFileTransfer, onUpgrade }
    // 文件传输状态缓存 key=agentId:path:action -> {total, done}
    this.transferState = new Map()
    this.onFileTransfer = (st) => {
      const key = `${st.agent_id}:${st.file_path}:${st.action}`
      if (st.status === 'transferring') {
        this.transferState.set(key, { total: st.total_bytes || 0, done: st.done_bytes || 0, status: 'transferring' })
      } else {
        this.transferState.set(key, { total: st.total_bytes || 0, done: st.done_bytes || 0, status: st.status, message: st.message })
      }
      onFileTransfer?.(st)
    }
    this.onUpgrade = onUpgrade
    this.reconnectDelay = 1000
    this.ws = null
    this.alive = true
  }

  connect() {
    this.ws = new WebSocket(this.url(), this.protocols)
    this.ws.onopen = () => {
      this.reconnectDelay = 1000
      this.onStatus('connected')
    }
    this.ws.onmessage = (ev) => {
      let msg
      try { msg = JSON.parse(ev.data) } catch { return }
      // 服务端 Envelope.Data 为 []byte，json 序列化后是 base64 字符串；
      // 用 TextDecoder 按 UTF-8 还原字节，避免中文等非 ASCII 出现乱码。
      let payload = msg.data
      if (typeof payload === 'string') {
        try {
          payload = JSON.parse(payload)
        } catch {
          try {
            const bin = atob(payload)
            const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
            payload = JSON.parse(new TextDecoder('utf-8').decode(bytes))
          } catch { payload = msg.data }
        }
      }
      switch (msg.type) {
        case 'AgentList': this.handlers.onAgentList?.(payload); break
        case 'CommandResp': this.handlers.onCommandResp?.(payload); break
        case 'InteractiveOutput': this.handlers.onInteractive?.(payload); break
        case 'PortMapState': this.handlers.onPortMap?.(payload); break
        case 'FileTransferState': this.handlers.onFileTransfer?.(payload); break
        case 'UpgradeResp': this.handlers.onUpgrade?.(payload); break
        default: break
      }
    }
    this.ws.onclose = () => {
      this.onStatus('disconnected')
      if (!this.alive) return
      setTimeout(() => {
        this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000)
        this.connect()
      }, this.reconnectDelay)
    }
    this.ws.onerror = () => this.ws && this.ws.close()
  }

  // 发送交互输入到指定 Agent；opts 可携带 { rows, cols } 同步终端尺寸
  sendInteractive(agentId, commandId, data, opts) {
    const payload = { agent_id: agentId, command_id: commandId, input_data: data }
    if (opts && opts.rows > 0 && opts.cols > 0) {
      payload.rows = opts.rows
      payload.cols = opts.cols
    }
    this.send({ type: 'InteractiveInputReq', data: payload })
  }

  // 文件分发：将本地文件分片上传到服务端，服务端转发给目标 Agent
  async sendFilePush({ agentId, commandId, filePath, file, onProgress }) {
    const CHUNK = 64 * 1024
    const total = file.size
    let seq = 0
    const totalChunks = Math.ceil(total / CHUNK)
    for (let offset = 0; offset < total; offset += CHUNK) {
      const slice = file.slice(offset, offset + CHUNK)
      const buf = new Uint8Array(await slice.arrayBuffer())
      // 将二进制转为 base64 便于 JSON 传输
      const b64 = this.bytesToBase64(buf)
      // 计算分片 MD5，供服务端做完整性校验（对应 FilePushReq.ChunkMD5）
      const md5 = this.md5Hex(buf)
      const done = offset + CHUNK >= total
      this.send({
        type: 'FilePushReq',
        data: {
          command_id: commandId,
          agent_id: agentId,
          file_path: filePath,
          file_size: total,
          chunk_seq: seq,
          chunk_data: b64,
          chunk_md5: md5,
          done,
        },
      })
      seq++
      onProgress?.({ done: offset + buf.length, total, percent: Math.round(((offset + buf.length) / total) * 100), chunks: `${seq}/${totalChunks}` })
      // 轻微限速，避免一次性塞满队列
      await new Promise((r) => setTimeout(r, 5))
    }
  }

  bytesToBase64(bytes) {
    let binary = ''
    const len = bytes.length
    for (let i = 0; i < len; i++) binary += String.fromCharCode(bytes[i])
    return btoa(binary)
  }

  // md5Hex 计算字节数组的 MD5（小写十六进制），与服务端 crypto/md5 一致。
  // 使用纯 JS 实现，避免额外依赖。
  md5Hex(bytes) {
    return this.md5Bytes(bytes)
  }

  // 精简 MD5 实现（RFC 1321），返回小写十六进制。
  md5Bytes(input) {
    const K = [
      0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee,
      0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
      0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
      0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
      0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa,
      0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
      0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
      0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
      0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
      0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
      0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05,
      0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
      0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039,
      0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
      0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
      0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391,
    ]
    const S = [
      7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
      5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
      4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
      6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
    ]
    // 预填充为 LE 的 512-bit 消息
    const msg = new Uint8Array(input)
    const bitLen = msg.length * 8
    // 补位：0x80 + 填充 0 + 64bit 长度
    const paddedLen = (((msg.length + 8) >> 6) + 1) << 6
    const padded = new Uint8Array(paddedLen)
    padded.set(msg)
    padded[msg.length] = 0x80
    // 64 位长度（低 32 位在前）
    const low = bitLen >>> 0
    const high = Math.floor(bitLen / 0x100000000)
    const dv = new DataView(padded.buffer)
    dv.setUint32(paddedLen - 8, low, true)
    dv.setUint32(paddedLen - 4, high, true)

    let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476

    const rotl = (x, c) => (x << c) | (x >>> (32 - c))
    const add = (x, y) => (x + y) >>> 0
    const add4 = (a, b, c, d) => add(add(add(a, b), c), d)

    for (let i = 0; i < paddedLen; i += 64) {
      const M = new Array(16)
      for (let j = 0; j < 16; j++) {
        M[j] = dv.getUint32(i + j * 4, true)
      }
      let A = a0, B = b0, C = c0, D = d0
      for (let j = 0; j < 64; j++) {
        let F, g
        if (j < 16) { F = (B & C) | (~B & D); g = j }
        else if (j < 32) { F = (D & B) | (~D & C); g = (5 * j + 1) % 16 }
        else if (j < 48) { F = B ^ C ^ D; g = (3 * j + 5) % 16 }
        else { F = C ^ (B | ~D); g = (7 * j) % 16 }
        F = add4(F, A, K[j], M[g]) >>> 0
        A = D
        D = C
        C = B
        B = add(B, rotl(F, S[j])) >>> 0
      }
      a0 = add(a0, A) >>> 0
      b0 = add(b0, B) >>> 0
      c0 = add(c0, C) >>> 0
      d0 = add(d0, D) >>> 0
    }
    const hex = (n) => {
      let s = ''
      for (let i = 0; i < 4; i++) {
        s += ((n >>> (i * 8)) & 0xff).toString(16).padStart(2, '0')
      }
      return s
    }
    return hex(a0) + hex(b0) + hex(c0) + hex(d0)
  }

  base64ToBytes(b64) {
    const binary = atob(b64)
    const len = binary.length
    const bytes = new Uint8Array(len)
    for (let i = 0; i < len; i++) bytes[i] = binary.charCodeAt(i)
    return bytes
  }

  send(obj) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj))
    }
  }

  close() {
    this.alive = false
    this.ws && this.ws.close()
  }

  onStatus() {}
}
