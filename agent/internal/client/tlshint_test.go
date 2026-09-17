package client

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestIsSANMismatch 判断"证书地址不匹配"这一特定错误。
// 只有它需要把运维引向「在服务端重签证书」—— 别的 TLS 失败多半是 Agent 侧配置问题。
func TestIsSANMismatch(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// 线上最典型的形态：服务端证书只含 127.0.0.1，Agent 却用公网 IP 连接
			name: "san mismatch",
			err:  errors.New("tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, ::1, not 82.156.7.85"),
			want: true,
		},
		{
			name: "wildcard vs single-name",
			err:  errors.New("x509: cannot validate certificate for 1.2.3.4 because it doesn't contain any IP SANs"),
			want: true,
		},
		{name: "connection refused", err: errors.New("dial tcp 1.2.3.4:9000: connect: connection refused"), want: false},
		{name: "unknown ca", err: errors.New("x509: certificate signed by unknown authority"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSANMismatch(c.err); got != c.want {
				t.Errorf("isSANMismatch(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestTLSFailurePointsToServerResign SAN 不匹配时，错误信息必须把运维引向
// "在服务端重签证书"这条真正有效的路径 —— 在被控机上改配置是修不好的。
func TestTLSFailurePointsToServerResign(t *testing.T) {
	c := &Client{cfg: Config{AgentID: "h1"}}
	cause := errors.New("tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, ::1, not 82.156.7.85")
	got := c.tlsFailure(cause).Error()

	// 原始错误必须完整保留，便于搜索与比对
	if !strings.Contains(got, "certificate is valid for") {
		t.Errorf("应保留原始 x509 错误，实际: %s", got)
	}
	for _, want := range []string{"scripts/signcert", "服务端证书的 SAN", "server-fingerprint"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示缺少 %q，实际: %s", want, got)
		}
	}
	// 这是服务端侧的问题，不该给出"删本机 creds 就会好"的误导
	if strings.Contains(got, "删除") && strings.Contains(got, "credDir") {
		t.Log("提示中同时出现删除 creds 的建议（用于 CA 轮换场景）——可接受")
	}
}

// TestTLSFailureKeepsUnrelatedErrorClean 非 SAN 类错误不该被塞进无关提示。
func TestTLSFailureKeepsUnrelatedErrorClean(t *testing.T) {
	c := &Client{cfg: Config{AgentID: "h1"}}
	cause := errors.New("dial tcp 1.2.3.4:9000: connect: connection refused")
	got := c.tlsFailure(cause).Error()
	if strings.Contains(got, "scripts/signcert") {
		t.Errorf("非 SAN 错误不该提示重签证书，实际: %s", got)
	}
	if !errors.Is(c.tlsFailure(cause), cause) {
		t.Error("包装后应可用 errors.Is 解出底层错误")
	}
}

// TestTLSFailureWrapsWithW 确认 %w 链路完整（外层能解出底层错误）。
func TestTLSFailureWrapsWithW(t *testing.T) {
	c := &Client{cfg: Config{AgentID: "h1"}}
	cause := errors.New("x509: certificate is valid for 127.0.0.1, not 82.156.7.85")
	wrapped := c.tlsFailure(cause)
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is 应能还原底层错误")
	}
	if !strings.Contains(fmt.Sprint(wrapped), "82.156.7.85") {
		t.Errorf("错误信息应保留地址细节，实际: %s", wrapped)
	}
}
