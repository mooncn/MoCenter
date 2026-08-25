import React, { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Api } from '../api/api.js'

export default function Login() {
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const nav = useNavigate()

  const doLogin = async (e) => {
    e.preventDefault()
    setErr('')
    if (!token.trim()) {
      setErr('请输入访问 Token')
      return
    }
    try {
      localStorage.setItem('mc_token', token.trim())
      await Api.agents() // 用该 token 尝试拉取主机列表以校验
      nav('/')
    } catch (ex) {
      localStorage.removeItem('mc_token')
      setErr('Token 无效或服务不可达')
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-box" onSubmit={doLogin}>
        <h1>🖥 MoCenter</h1>
        <div className="sub">集中式主机远控管理系统</div>
        {err && <div className="notice err">{err}</div>}
        <div className="form-row">
          <label>访问 Token</label>
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="请输入服务端 API Key"
          />
        </div>
        <button style={{ width: '100%' }} type="submit">登录</button>
        <div style={{ marginTop: 16, color: '#8b98a5', fontSize: 13 }}>
          默认 Token 为服务端启动时的 <code>-apikey</code> 参数值
        </div>
      </form>
    </div>
  )
}
