import React, { useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { Api, PublicApi } from '../api/api.js'

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [otp, setOtp] = useState('')
  const [err, setErr] = useState('')
  // 公开状态页是否启用：启用时给未登录访客一个入口（无信息泄露，只查布尔值）
  const [publicEnabled, setPublicEnabled] = useState(false)
  const nav = useNavigate()
  const [searchParams] = useSearchParams()
  const redirectTo = searchParams.get('redirect') || '/'

  useEffect(() => {
    let alive = true
    PublicApi.enabled().then((v) => alive && setPublicEnabled(v)).catch(() => {})
    return () => { alive = false }
  }, [])

  const doLogin = async (e) => {
    e.preventDefault()
    setErr('')
    if (!username.trim()) {
      setErr('请填写用户名')
      return
    }
    try {
      const res = await Api.login({ username: username.trim(), password, otp: otp.trim() })
      if (res && res.ok && res.token) {
        localStorage.setItem('mc_token', res.token)
        nav(redirectTo)
        return
      }
      setErr((res && res.msg) || '登录失败')
    } catch (ex) {
      setErr(ex.response?.data?.msg || ex.message)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-box" onSubmit={doLogin}>
        <h1>🖥 MoCenter</h1>
        <div className="sub">集中式主机远控管理系统</div>
        {err && <div className="notice err">{err}</div>}
        <div className="form-row">
          <label>用户名</label>
          <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="如 admin" autoFocus />
        </div>
        <div className="form-row">
          <label>密码</label>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="登录密码" />
        </div>
        <div className="form-row">
          <label>动态验证码（OTP，可选）</label>
          <input value={otp} onChange={(e) => setOtp(e.target.value)} placeholder="Google Authenticator 6 位码，未启用可留空" />
        </div>
        <button className="btn-block" style={{ marginTop: 4 }} type="submit">登录</button>
        {publicEnabled && (
          <a className="public-entry" href="/public">查看公开节点状态 →</a>
        )}
        <div className="sub" style={{ marginTop: 16, color: '#8b98a5', fontSize: 13 }}>
          使用服务端配置的账号登录（<code>-user</code>/<code>-password</code>，首次启动会自动创建
          <code>admin</code>/<code>123456</code>）；若账号绑定了 OTP，需一并填写 6 位动态验证码。
        </div>
      </form>
    </div>
  )
}