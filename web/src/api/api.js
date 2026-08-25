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
