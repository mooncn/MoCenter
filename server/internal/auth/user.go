package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 登录与改密相关错误。
var (
	ErrNoUsers         = errors.New("未配置可登录用户")
	ErrBadLogin        = errors.New("用户名或密码错误")
	ErrBadOTP          = errors.New("一次性验证码无效")
	ErrBadUsername     = errors.New("用户不存在")
	ErrBadOldPassword  = errors.New("当前密码错误")
	ErrBadNewPassword  = errors.New("新密码为空或过短")
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
	path  string
}

// NewUserManager 创建基于指定文件持久化的用户管理器。
func NewUserManager(path string) *UserManager {
	return &UserManager{users: map[string]*User{}, path: path}
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
func (m *UserManager) Save() error {
	if m.path == "" {
		return nil
	}
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	payload := struct {
		Users map[string]*User `json:"users"`
	}{Users: m.users}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0644)
}

// InitFromFlags 首次引导：从命令行参数创建初始管理员账号。
// 若文件已存在账号则以文件为准（忽略 flag），否则在 username 非空时创建并持久化。
func (m *UserManager) InitFromFlags(username, password, totpSecret string) error {
	if err := m.Load(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) == 0 && username != "" {
		m.users[username] = &User{
			Username:     username,
			PasswordHash: hashPassword(password),
			TOTPSecret:   strings.TrimSpace(totpSecret),
		}
		return m.Save()
	}
	return nil
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
func (m *UserManager) Authenticate(username, password, otp string) error {
	m.mu.RLock()
	u, ok := m.users[username]
	m.mu.RUnlock()
	if !ok {
		return ErrBadLogin
	}
	if subtle.ConstantTimeCompare([]byte(u.PasswordHash), []byte(hashPassword(password))) != 1 {
		return ErrBadLogin
	}
	if u.TOTPSecret != "" {
		if !validateTOTP(u.TOTPSecret, otp, 1) {
			return ErrBadOTP
		}
	}
	return nil
}

// ChangePassword 修改用户密码（需校验当前密码，若启用 OTP 还需验证码），并持久化。
func (m *UserManager) ChangePassword(username, oldPassword, newPassword, otp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return ErrBadUsername
	}
	if subtle.ConstantTimeCompare([]byte(u.PasswordHash), []byte(hashPassword(oldPassword))) != 1 {
		return ErrBadOldPassword
	}
	if newPassword == "" || len(newPassword) < 6 {
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

func hashPassword(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])
}