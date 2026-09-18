import React, { useEffect, useState } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import { Api, PublicApi } from '../api/api.js'
// 登录成功后的默认落地页：**后台仪表盘**（不是公开状态页）
const DEFAULT_REDIRECT = '/admin'

// 只允许站内相对路径作为回跳目标 —— 否则 /admin/login?redirect=https://evil.com
// 就成了一个开放重定向（钓鱼常用手法：链接域名是对的，跳过去是假的登录页）。
// 规则：必须以单个 '/' 开头，且不能是 '//' 或 '/\'（协议相对地址）。
function safeRedirect(raw) {
  if (!raw) return DEFAULT_REDIRECT
  if (!raw.startsWith('/')) return DEFAULT_REDIRECT
  if (raw.startsWith('//') || raw.startsWith('/\\')) return DEFAULT_REDIRECT
  return raw
}

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [otp, setOtp] = useState('')
  const [err, setErr] = useState('')
  // 公开状态页是否启用：启用时给未登录访客一个入口（无信息泄露，只查布尔值）
  const [publicEnabled, setPublicEnabled] = useState(false)
  // 会话校验结果：null=校验中 true=已登录 false=未登录
  // **必须三态**：只看"本地有没有 token"是不够的（token 过期/被服务端吊销时依然存在），
  // 但直接往前端判断也会把"还没校验完"误判成"未登录"，于是已登录用户会看到登录表单闪一下。
  const [loggedIn, setLoggedIn] = useState(null)
  const nav = useNavigate()
  const [searchParams] = useSearchParams()
  const redirectTo = safeRedirect(searchParams.get('redirect'))

  // 已登录访问 /admin/login 直接跳转（不再要求重新登录一次）。
  // 校验方式：带本地 token 调 /api/agents —— 服务端认这个 token 才算已登录。
  // 401 会被 api.js 的响应拦截器清掉 token，所以失败后这里就是"未登录"的干净状态。
  useEffect(() => {
    let alive = true
    const token = localStorage.getItem('mc_token')
    if (!token) {
      setLoggedIn(false)
      return () => { alive = false }
    }
    Api.agents()
      .then(() => { if (alive) setLoggedIn(true) })
      .catch(() => { if (alive) setLoggedIn(false) })
    return () => { alive = false }
  }, [])

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
        nav(redirectTo, { replace: true })
        return
      }
      setErr((res && res.msg) || '登录失败')
    } catch (ex) {
      setErr(ex.response?.data?.msg || ex.message)
    }
  }

  // 校验完成且确认已登录：直接进后台（或回跳原地址）。
  // 用 replace 不留历史记录 —— 否则用户按"返回"会又回到登录页，再被弹回后台，来回死循环。
  if (loggedIn === true) {
    return <Navigate to={redirectTo} replace />
  }

  // 校验中：保持登录态（既不闪表单，也不闪跳转）。
  // 这一屏同时保证"已登录时不会看到登录表单"，因为表单只在确认未登录后才渲染。
  if (loggedIn === null) {
    return (
      <div className="login-wrap">
        <div className="login-box">
          <h1>🖥 MoCenter</h1>
          <div className="sub">正在检查登录状态…</div>
        </div>
      </div>
    )
  }

  return (
    <div className="login-wrap">
      <form className="login-box" onSubmit={doLogin}>
        <h1>🖥 MoCenter</h1>
        <div className="sub">集中式主机远控管理系统</div>
        {err && <div className="notice err">{err}</div>}
        <div className="form-row">
          <label>用户名</label>
          <input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="如 admin"
            autoComplete="username"
            autoFocus
          />
        </div>
        <div className="form-row">
          <label>密码</label>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="登录密码"
            autoComplete="current-password"
          />
        </div>
        <div className="form-row">
          <label>动态验证码（OTP，可选）</label>
          <input
            value={otp}
            onChange={(e) => setOtp(e.target.value)}
            placeholder="Google Authenticator 6 位码，未启用可留空"
            inputMode="numeric"
            autoComplete="one-time-code"
          />
        </div>
        <button className="btn-block" style={{ marginTop: 4 }} type="submit">登录</button>
        {publicEnabled && (
          <a className="public-entry" href="/public">查看公开节点状态 →</a>
        )}
        <div className="sub" style={{ marginTop: 16, color: '#8b98a5', fontSize: 13 }}>
          使用服务端配置的账号登录（<code>-user</code>/<code>-password</code>，首次启动会自动创建
          <code>admin</code>，初始口令见数据目录下的 <code>initial-password.txt</code>）；若账号绑定了
          OTP，需一并填写 6 位动态验证码。
        </div>
      </form>
    </div>
  )
}
