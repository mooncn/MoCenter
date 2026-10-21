package auth

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 登录与改密相关错误。
var (
	ErrNoUsers        = errors.New("未配置可登录用户")
	ErrBadLogin       = errors.New("用户名或密码错误")
	ErrBadOTP         = errors.New("一次性验证码无效")
	ErrBadUsername    = errors.New("用户不存在")
	ErrBadOldPassword = errors.New("当前密码错误")
	ErrBadNewPassword = errors.New("新密码为空或过短")
)

// User 一个可登录账号。
type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	TOTPSecret   string `json:"totp_secret,omitempty"` // 为空表示未启用 OTP
}

// UserManager 管理可登录账号；账号持久化到 JSON 文件（密码以哈希存储）。
type UserManager struct {
	mu    sync.RWMutex
	users map[string]*User
	// otpLast 记录每个用户最近一次通过校验的 TOTP 时间步，用于拒绝重放：
	// 同一窗口内（含 ±1 步容差）的验证码只能用一次，否则截获者可在有效期内复用。
	otpLast map[string]int64
	path    string
}

// NewUserManager 创建基于指定文件持久化的用户管理器。
func NewUserManager(path string) *UserManager {
	return &UserManager{
		users:   map[string]*User{},
		otpLast: map[string]int64{},
		path:    path,
	}
}

// Load 从文件加载账号（文件不存在则保持为空）。
func (m *UserManager) Load() error {
	if m.path == "" {
		return nil
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload struct {
		Users map[string]*User `json:"users"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return err
	}
	if payload.Users == nil {
		payload.Users = map[string]*User{}
	}
	m.mu.Lock()
	m.users = payload.Users
	m.mu.Unlock()
	return nil
}

// Save 将账号持久化到文件。
//
// 权限 0600：文件里有口令哈希与 OTP 密钥，属敏感数据，不能让同机其他用户读到。
func (m *UserManager) Save() error {
	if m.path == "" {
		return nil
	}
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}
	payload := struct {
		Users map[string]*User `json:"users"`
	}{Users: m.users}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0600)
}

// ErrInitPasswordRequired 首次创建账号时未提供密码。
var ErrInitPasswordRequired = errors.New("创建账号必须提供非空密码（-password）")

// emptyPasswordHash 是空字符串的 SHA-256，用于识别历史上以空密码误建的账号。
const emptyPasswordHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// InitFromFlags 首次引导：从命令行参数创建初始管理员账号。
// 若文件已存在账号则以文件为准（忽略 flag），否则在 username 非空时创建并持久化。
// 注意：password 为空时会拒绝创建，避免生成「任何密码都校验失败/空密码可登录」的坏账号。
func (m *UserManager) InitFromFlags(username, password, totpSecret string) error {
	if err := m.Load(); err != nil {
		return err
	}
	if username == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return nil
	}
	if strings.TrimSpace(password) == "" {
		return ErrInitPasswordRequired
	}
	m.users[username] = &User{
		Username:     username,
		PasswordHash: hashPassword(password),
		TOTPSecret:   strings.TrimSpace(totpSecret),
	}
	return m.Save()
}

// BrokenAccounts 返回密码哈希为空字符串哈希的账号名列表。
// 这些账号由早期版本在未提供 -password 时误建，任何非空密码都无法登录。
func (m *UserManager) BrokenAccounts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for name, u := range m.users {
		if u.PasswordHash == emptyPasswordHash {
			out = append(out, name)
		}
	}
	return out
}

// SetPassword 直接为用户设置新密码（不校验旧密码）。
// 供「修复历史空密码账号」与「首次初始化」使用；调用方需自行确保授权。
func (m *UserManager) SetPassword(username, newPassword string) error {
	if strings.TrimSpace(newPassword) == "" {
		return ErrBadNewPassword
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	u.PasswordHash = hashPassword(newPassword)
	return m.Save()
}

// passwordIsEmptyHash 判断哈希是否为「空密码」哈希。
func passwordIsEmptyHash(h string) bool { return h == emptyPasswordHash }

// UpsertUser 创建或更新账号（不存在则创建），用于初始化/修复历史坏账号。
func (m *UserManager) UpsertUser(username, password, totpSecret string) error {
	if strings.TrimSpace(username) == "" {
		return ErrBadUsername
	}
	if strings.TrimSpace(password) == "" {
		return ErrInitPasswordRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[username]; ok {
		u.PasswordHash = hashPassword(password)
		if strings.TrimSpace(totpSecret) != "" {
			u.TOTPSecret = strings.TrimSpace(totpSecret)
		}
	} else {
		m.users[username] = &User{
			Username:     username,
			PasswordHash: hashPassword(password),
			TOTPSecret:   strings.TrimSpace(totpSecret),
		}
	}
	return m.Save()
}

// TOTPStatus 返回用户的 OTP 启用状态。
// 第二个返回值表示账号是否存在。
func (m *UserManager) TOTPStatus(username string) (enabled, exists bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[username]
	if !ok {
		return false, false
	}
	return u.TOTPSecret != "", true
}

// BindTOTP 在验证码校验通过后为用户启用 OTP（绑定密钥）。
func (m *UserManager) BindTOTP(username, secret string) error {
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	if secret == "" {
		return ErrBadOTP
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	u.TOTPSecret = secret
	// 换密钥后旧的重放水位没有意义（时间步与旧密钥绑定）。
	delete(m.otpLast, username)
	return m.Save()
}

// UnbindTOTP 为用户关闭 OTP（清除密钥）。需校验当前密码，若已启用 OTP 还需验证码。
func (m *UserManager) UnbindTOTP(username, password, otp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	if ok, _ := verifyPassword(u.PasswordHash, password); !ok {
		return ErrBadLogin
	}
	if u.TOTPSecret != "" && !validateTOTP(u.TOTPSecret, otp, 1) {
		return ErrBadOTP
	}
	u.TOTPSecret = ""
	delete(m.otpLast, username)
	return m.Save()
}

// VerifyPassword 校验当前密码，用于敏感操作（重新生成/关闭 OTP）前的二次确认。
//
// 只校验密码本身，不涉及 OTP 与登录态 —— 越是"能改变安全配置"的操作，
// 越不能只凭一枚可能被窃取的会话 Token 放行。
func (m *UserManager) VerifyPassword(username, password string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	if ok, _ := verifyPassword(u.PasswordHash, password); !ok {
		return ErrBadOldPassword
	}
	return nil
}

// VerifyTOTP 校验用户当前 OTP 密钥对应的验证码（用于绑定前的自检）。
// 若用户尚未设置密钥，返回 false。
func (m *UserManager) VerifyTOTP(username, secret, code string) bool {
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	if secret == "" {
		return false
	}
	return validateTOTP(secret, code, 1)
}

// GenerateTOTPSecret 生成一个新的 base32 TOTP 密钥（160 bit，兼容 Google Authenticator）。
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// TOTPUri 生成 otpauth:// URI，供前端生成二维码。
func TOTPUri(issuer, account, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		url.PathEscape(issuer), url.PathEscape(account), secret, url.QueryEscape(issuer))
}

// HasUsers 是否存在可登录用户。
func (m *UserManager) HasUsers() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users) > 0
}

// EnabledOTP 该用户是否启用 OTP 二次校验。
func (m *UserManager) EnabledOTP(username string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if u, ok := m.users[username]; ok {
		return u.TOTPSecret != ""
	}
	return false
}

// Authenticate 校验用户名+密码，以及（若启用）OTP 验证码。
//
// 若该账号仍存着旧版单轮 SHA-256 哈希，校验通过后**透明升级**为 PBKDF2，
// 因此老用户不需要改密即可获得新的强度。
func (m *UserManager) Authenticate(username, password, otp string) error {
	m.mu.RLock()
	u, ok := m.users[username]
	m.mu.RUnlock()
	if !ok {
		// 走一次同成本的口令校验：用户不存在与口令错误的耗时一致，避免枚举账号。
		_, _ = verifyPassword(dummyPasswordHash, password)
		return ErrBadLogin
	}
	passOK, needsUpgrade := verifyPassword(u.PasswordHash, password)
	if !passOK {
		return ErrBadLogin
	}
	if u.TOTPSecret != "" {
		step, ok := validateTOTPStep(u.TOTPSecret, otp, 1)
		if !ok {
			return ErrBadOTP
		}
		m.mu.Lock()
		if m.otpLast == nil {
			m.otpLast = map[string]int64{}
		}
		if last, seen := m.otpLast[username]; seen && step <= last {
			m.mu.Unlock()
			return ErrBadOTP
		}
		m.otpLast[username] = step
		m.mu.Unlock()
	}
	if needsUpgrade {
		m.upgradeHash(username, password)
	}
	return nil
}

// upgradeHash 把旧格式口令哈希改写为新格式（仅在口令校验通过后调用）。
func (m *UserManager) upgradeHash(username, password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return
	}
	// 双重检查：期间可能已被改密为新格式
	if _, needs := verifyPassword(u.PasswordHash, password); !needs {
		return
	}
	u.PasswordHash = hashPassword(password)
	if err := m.Save(); err != nil {
		// 升级失败不影响本次登录；下次登录会再试一次
		return
	}
}

// ChangePassword 修改用户密码（需校验当前密码，若启用 OTP 还需验证码），并持久化。
func (m *UserManager) ChangePassword(username, oldPassword, newPassword, otp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	if ok, _ := verifyPassword(u.PasswordHash, oldPassword); !ok {
		return ErrBadOldPassword
	}
	if !isStrongPassword(newPassword) {
		return ErrBadNewPassword
	}
	if u.TOTPSecret != "" {
		if !validateTOTP(u.TOTPSecret, otp, 1) {
			return ErrBadOTP
		}
	}
	u.PasswordHash = hashPassword(newPassword)
	return m.Save()
}
