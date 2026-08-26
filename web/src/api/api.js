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

export const Api = {
  // 获取全部主机（含离线）
  async agents() {
    const { data } = await api.get('/agents')
    return data
  },
  // 获取主机历史探针趋势
  async history(agent, since) {
    const { data } = await api.get('/agents/history', { params: { agent, since } })
    return data
  },
  // 下发命令
  async command(req) {
    const { data } = await api.post('/command', req)
    return data
  },
  // 审计日志
  async audits() {
    const { data } = await api.get('/audits')
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
}

// WebSocket 客户端：用于接收服务端实时推送（主机状态、命令输出等）。
export class MCSocket {
  constructor({ onAgentList, onCommandResp, onInteractive, onPortMap, onFileTransfer, onUpgrade, token }) {
    this.url = () => {
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      return `${proto}://${location.host}/ws?token=${encodeURIComponent(token || '')}`
    }
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
    this.ws = new WebSocket(this.url())
    this.ws.onopen = () => {
      this.reconnectDelay = 1000
      this.onStatus('connected')
    }
    this.ws.onmessage = (ev) => {
      let msg
      try { msg = JSON.parse(ev.data) } catch { return }
      const payload = msg.data ? (typeof msg.data === 'string' ? JSON.parse(msg.data) : msg.data) : msg
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

  // 发送交互输入到指定 Agent
  sendInteractive(agentId, commandId, data) {
    this.send({ type: 'InteractiveInputReq', data: { agent_id: agentId, command_id: commandId, input_data: data } })
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
