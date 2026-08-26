import React, { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Api } from '../api/api.js'

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [otp, setOtp] = useState('')
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const nav = useNavigate()

  const doLogin = async (e) => {
    e.preventDefault()
    setErr('')
    // 方式一：直接使用访问 Token（= 服务端 -apikey），最常用、无需账号
    if (token.trim()) {
      try {
        localStorage.setItem('mc_token', token.trim())
        await Api.agents()
        nav('/')
        return
      } catch {
        localStorage.removeItem('mc_token')
        setErr('Token 无效或服务不可达')
        return
      }
    }
    // 方式二：用户名 + 密码（+ OTP，若服务端启用了账号与二次校验）
    if (username.trim()) {
      try {
        const res = await Api.login({ username: username.trim(), password, otp: otp.trim() })
        if (res && res.ok && res.token) {
          localStorage.setItem('mc_token', res.token)
          nav('/')
          return
        }
        setErr((res && res.msg) || '登录失败')
      } catch (ex) {
        setErr(ex.response?.data?.msg || ex.message)
      }
      return
    }
    setErr('请填写访问 Token，或用户名/密码')
  }

  return (
    <div className="login-wrap">
      <form className="login-box" onSubmit={doLogin}>
        <h1>🖥 MoCenter</h1>
        <div className="sub">集中式主机远控管理系统</div>
        {err && <div className="notice err">{err}</div>}
        <div className="form-row">
          <label>访问 Token / API Key</label>
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="服务端 -apikey 值（未指定则为 change-me-api-key）"
          />
        </div>
        <div className="sub" style={{ color: '#8b98a5', fontSize: 12, marginBottom: 4 }}>以下为可选－若服务端启用了账号（-user/-password）再用以下方式登录</div>
        <div className="form-row">
          <label>用户名</label>
          <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="如 admin" />
        </div>
        <div className="form-row">
          <label>密码</label>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="登录密码" />
        </div>
        <div className="form-row">
          <label>动态验证码（OTP，可选）</label>
          <input value={otp} onChange={(e) => setOtp(e.target.value)} placeholder="Google Authenticator 6 位码，未启用可留空" />
        </div>
        <button style={{ width: '100%', marginTop: 4 }} type="submit">登录</button>
        <div className="sub" style={{ marginTop: 16, color: '#8b98a5', fontSize: 13 }}>
          默认登录：填写服务端启动时的 <code>-apikey</code> 作为访问 Token；系统默认无内置账号密码。
          若服务端配置了 <code>-user/-password</code>，也可用用户名+密码（及可选 OTP）登录。
        </div>
      </form>
    </div>
  )
}