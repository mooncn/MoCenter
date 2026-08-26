import React, { useState } from 'react'
import { Api } from '../api/api.js'

// 账号设置：修改登录用户名对应的密码
export default function Account() {
  const [username, setUsername] = useState('')
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [otp, setOtp] = useState('')
  const [notice, setNotice] = useState('')
  const [ok, setOk] = useState(false)

  const submit = async (e) => {
    e.preventDefault()
    setNotice('')
    setOk(false)
    if (!username.trim() || !oldPassword || !newPassword) {
      setNotice('请填写用户名、当前密码与新密码')
      return
    }
    if (newPassword !== confirm) {
      setNotice('两次输入的新密码不一致')
      return
    }
    if (newPassword.length < 6) {
      setNotice('新密码长度至少 6 位')
      return
    }
    try {
      const res = await Api.changePassword({
        username: username.trim(),
        old_password: oldPassword,
        new_password: newPassword,
        otp: otp.trim(),
      })
      if (res && res.ok) {
        setNotice((res.msg) || '密码修改成功')
        setOk(true)
        setOldPassword('')
        setNewPassword('')
        setConfirm('')
        setOtp('')
      } else {
        setNotice((res && res.msg) || '修改失败')
      }
    } catch (ex) {
      setNotice(ex.response?.data?.msg || ex.message)
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>账号设置</h2>
          <div className="sub">修改 Web 后台登录密码（启用账号登录时才需要）</div>
        </div>
      </div>

      <div className="card" style={{ maxWidth: 520 }}>
        <form onSubmit={submit}>
          <div className="form-row">
            <label>用户名</label>
            <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="输入你的登录用户名" />
          </div>
          <div className="form-row">
            <label>当前密码</label>
            <input type="password" value={oldPassword} onChange={(e) => setOldPassword(e.target.value)} placeholder="当前登录密码" />
          </div>
          <div className="form-row">
            <label>新密码</label>
            <input type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} placeholder="至少 6 位" />
          </div>
          <div className="form-row">
            <label>确认新密码</label>
            <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder="再次输入新密码" />
          </div>
          <div className="form-row">
            <label>动态验证码（OTP，可选）</label>
            <input value={otp} onChange={(e) => setOtp(e.target.value)} placeholder="若该账号启用了 OTP 则必填" />
          </div>
          <button type="submit">修改密码</button>
        </form>
        {notice && <div className={`notice ${ok ? 'ok' : 'err'}`} style={{ marginTop: 12 }}>{notice}</div>}
        <div className="sub" style={{ marginTop: 12, color: '#8b98a5', fontSize: 13 }}>
          系统默认无内置账号；使用 API Key（访问 Token）登录的角色无需修改密码。
          密码以哈希形式存储于服务端 users.json，不会明文落盘。
        </div>
      </div>
    </div>
  )
}