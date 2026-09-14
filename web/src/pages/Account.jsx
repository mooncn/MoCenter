import React, { useEffect, useState } from 'react'
import { Api } from '../api/api.js'
import { createQrSvg } from '../utils/qrcode.js'

// 账号设置：修改登录密码 + OTP 动态验证码（扫码绑定/关闭）
export default function Account() {
  const [username, setUsername] = useState('')
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [otp, setOtp] = useState('')
  const [notice, setNotice] = useState('')
  const [ok, setOk] = useState(false)

  // OTP 绑定状态
  const [otpUser, setOtpUser] = useState('')
  const [otpEnabled, setOtpEnabled] = useState(false)
  const [otpSecret, setOtpSecret] = useState('')
  const [otpUri, setOtpUri] = useState('')
  const [otpCode, setOtpCode] = useState('')
  const [otpNotice, setOtpNotice] = useState('')
  const [otpOk, setOtpOk] = useState(false)
  const [otpPassword, setOtpPassword] = useState('')
  const [otpDisableCode, setOtpDisableCode] = useState('')

  // 初始化：沿用上次使用的用户名
  useEffect(() => {
    const saved = localStorage.getItem('mc_username')
    if (saved) {
      setUsername(saved)
      setOtpUser(saved)
    }
  }, [])

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
        setNotice(res.msg || '密码修改成功')
        setOk(true)
        localStorage.setItem('mc_username', username.trim())
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

  // 生成新密钥与二维码（此时尚未启用，需输入验证码确认）
  const startOtpSetup = async () => {
    setOtpNotice('')
    setOtpOk(false)
    if (!otpUser.trim()) {
      setOtpNotice('请先填写要绑定 OTP 的用户名')
      return
    }
    try {
      const res = await Api.otpSetup(otpUser.trim())
      if (!res || !res.ok) {
        setOtpNotice((res && res.msg) || '生成密钥失败')
        return
      }
      setOtpSecret(res.secret)
      setOtpUri(res.uri)
      setOtpEnabled(!!res.enabled)
      setOtpCode('')
      localStorage.setItem('mc_username', otpUser.trim())
    } catch (ex) {
      setOtpNotice(ex.response?.data?.msg || ex.message)
    }
  }

  // 扫码后输入验证码，比对正确即绑定启用
  const confirmOtp = async () => {
    setOtpNotice('')
    setOtpOk(false)
    if (!otpUser.trim() || !otpSecret) {
      setOtpNotice('请先生成二维码')
      return
    }
    if (!/^\d{6}$/.test(otpCode.trim())) {
      setOtpNotice('请输入 6 位动态验证码')
      return
    }
    try {
      const res = await Api.otpEnable({
        username: otpUser.trim(),
        secret: otpSecret,
        code: otpCode.trim(),
      })
      if (res && res.ok) {
        setOtpNotice(res.msg || 'OTP 已启用')
        setOtpOk(true)
        setOtpEnabled(true)
        setOtpSecret('')
        setOtpUri('')
        setOtpCode('')
      } else {
        setOtpNotice((res && res.msg) || '验证码不正确')
      }
    } catch (ex) {
      setOtpNotice(ex.response?.data?.msg || ex.message)
    }
  }

  // 关闭 OTP：需密码，启用中还需当前验证码
  const disableOtp = async () => {
    setOtpNotice('')
    setOtpOk(false)
    if (!otpUser.trim() || !otpPassword) {
      setOtpNotice('请填写用户名与当前密码')
      return
    }
    try {
      const res = await Api.otpDisable({
        username: otpUser.trim(),
        password: otpPassword,
        otp: otpDisableCode.trim(),
      })
      if (res && res.ok) {
        setOtpNotice(res.msg || 'OTP 已关闭')
        setOtpOk(true)
        setOtpEnabled(false)
        setOtpSecret('')
        setOtpUri('')
        setOtpPassword('')
        setOtpDisableCode('')
      } else {
        setOtpNotice((res && res.msg) || '关闭失败')
      }
    } catch (ex) {
      setOtpNotice(ex.response?.data?.msg || ex.message)
    }
  }

  const qrSvg = otpUri ? createQrSvg(otpUri, 200, 'M') : ''

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>账号设置</h2>
          <div className="sub">修改 Web 后台登录密码，并配置 OTP 动态验证码二次校验</div>
        </div>
      </div>

      <div className="card" style={{ maxWidth: 520 }}>
        <h3>修改密码</h3>
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
      </div>

      <div className="card" style={{ maxWidth: 520, marginTop: 16 }}>
        <h3>OTP 动态验证码</h3>
        <div className="desc">
          启用后登录需额外输入一次动态验证码（兼容 Google Authenticator / Microsoft Authenticator / 1Password 等）。
          点击「生成二维码」后用验证器扫码，再输入验证器当前显示的 6 位验证码完成绑定。
        </div>

        <div className="form-row">
          <label>用户名</label>
          <input value={otpUser} onChange={(e) => setOtpUser(e.target.value)} placeholder="要绑定 OTP 的登录用户名" />
        </div>

        {otpEnabled && !otpUri && (
          <div className="notice ok" style={{ marginBottom: 12 }}>
            当前状态：<strong>已启用</strong> OTP 动态验证码
          </div>
        )}

        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          <button type="button" onClick={startOtpSetup}>生成二维码</button>
          {otpEnabled && (
            <button type="button" className="secondary danger" onClick={() => { setOtpUri(''); setOtpSecret(''); setOtpNotice('') }}>
              重新绑定
            </button>
          )}
        </div>

        {otpUri && (
          <div style={{ marginTop: 16 }}>
            <div style={{ display: 'flex', gap: 16, alignItems: 'flex-start', flexWrap: 'wrap' }}>
              <div
                style={{ background: '#fff', padding: 8, borderRadius: 8, border: '1px solid #e5e7eb', lineHeight: 0 }}
                dangerouslySetInnerHTML={{ __html: qrSvg }}
              />
              <div style={{ flex: 1, minWidth: 200 }}>
                <div style={{ fontSize: 13, color: '#6b7280', marginBottom: 6 }}>无法扫码时手动输入密钥：</div>
                <div
                  style={{
                    fontFamily: 'Menlo, Consolas, monospace',
                    fontSize: 13,
                    background: '#f3f4f6',
                    padding: '8px 10px',
                    borderRadius: 6,
                    wordBreak: 'break-all',
                    marginBottom: 12,
                  }}
                >
                  {otpSecret}
                </div>
                <div className="form-row">
                  <label>验证器当前动态验证码</label>
                  <input
                    value={otpCode}
                    onChange={(e) => setOtpCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
                    placeholder="6 位数字"
                    maxLength={6}
                    inputMode="numeric"
                  />
                </div>
                <button type="button" className="primary" onClick={confirmOtp}>比对并启用</button>
              </div>
            </div>
          </div>
        )}

        {otpEnabled && (
          <div style={{ marginTop: 16, paddingTop: 16, borderTop: '1px solid #e5e7eb' }}>
            <div style={{ fontSize: 13, color: '#6b7280', marginBottom: 8 }}>
              关闭 OTP（需验证当前密码；已启用时还需输入一次动态验证码）
            </div>
            <div className="form-row">
              <label>当前密码</label>
              <input type="password" value={otpPassword} onChange={(e) => setOtpPassword(e.target.value)} placeholder="当前登录密码" />
            </div>
            <div className="form-row">
              <label>动态验证码</label>
              <input
                value={otpDisableCode}
                onChange={(e) => setOtpDisableCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
                placeholder="6 位数字"
                maxLength={6}
                inputMode="numeric"
              />
            </div>
            <button type="button" className="secondary danger" onClick={disableOtp}>关闭 OTP</button>
          </div>
        )}

        {otpNotice && <div className={`notice ${otpOk ? 'ok' : 'err'}`} style={{ marginTop: 12 }}>{otpNotice}</div>}

        <div className="sub" style={{ marginTop: 12, color: '#8b98a5', fontSize: 13 }}>
          密码与 OTP 密钥均以哈希/密文形式存储于服务端 users.json，不会明文落盘。
        </div>
      </div>
    </div>
  )
}
