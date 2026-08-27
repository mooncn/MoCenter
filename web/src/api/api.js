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
  // 用户名+密码（+OTP 可选）登录，成功返回 { token }
  async login({ username, password, otp }) {
    const { data } = await api.post('/login', { username, password, otp })
    return data
  },
  // 修改用户密码
  async changePassword(payload) {
    const { data } = await api.post('/password', payload)
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
  // 一键下发 VNC 安装/启动指令
  async vncInstall(payload) {
    const { data } = await api.post('/vnc/install', payload)
    return data
  },
  // 查询最近一次 VNC 安装/启动结果（含口令，供超时后补取）
  async vncStatus(agent) {
    const { data } = await api.get('/vnc/status', { params: { agent } })
    return data
  },
  // 设置/更新主机备注
  async setRemark(agentId, remark) {
    const { data } = await api.post('/agents/remark', { agent_id: agentId, remark })
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
