package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadPasswordFile 覆盖初始口令文件的解析与安全约束。
//
// 背景：口令原先通过 `-password` 传给服务端，而 systemd 的 ExecStart 参数
// 对任意本地用户可读（ps -ef / systemctl show -p ExecStart），等于口令泄露。
// 改为读 0600 文件后，"文件被读错"或"权限没收紧"都会让这套机制形同虚设，
// 因此这几条约束必须有测试兜住。
func TestReadPasswordFile(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatalf("准备文件 %s 失败: %v", name, err)
		}
		// WriteFile 受 umask 影响，显式再 chmod 一次
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s 失败: %v", name, err)
		}
		return p
	}

	t.Run("纯口令一行", func(t *testing.T) {
		p := write("plain", "S3cret-Pw!\n", 0600)
		got, err := readPasswordFile(p)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if got != "S3cret-Pw!" {
			t.Fatalf("口令 = %q，期望 %q", got, "S3cret-Pw!")
		}
	})

	t.Run("无结尾换行", func(t *testing.T) {
		p := write("nonl", "NoNewlinePw", 0600)
		got, _ := readPasswordFile(p)
		if got != "NoNewlinePw" {
			t.Fatalf("口令 = %q", got)
		}
	})

	t.Run("KEY=VALUE 形式", func(t *testing.T) {
		for _, key := range []string{"MOCENTER_PASSWORD", "PASSWORD", "ADMIN_PASSWORD", "SOME_PASSWORD"} {
			p := write("kv_"+key, "MOCENTER_USER=admin\n"+key+"=KvPw123\n", 0600)
			got, err := readPasswordFile(p)
			if err != nil {
				t.Fatalf("%s: 意外错误 %v", key, err)
			}
			if got != "KvPw123" {
				t.Fatalf("%s: 口令 = %q，期望 KvPw123", key, got)
			}
		}
	})

	t.Run("带引号与注释", func(t *testing.T) {
		p := write("quoted", "# 注释行\nMOCENTER_PASSWORD='QuotedPw'\n", 0600)
		got, err := readPasswordFile(p)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if got != "QuotedPw" {
			t.Fatalf("口令 = %q，期望 QuotedPw", got)
		}
	})

	t.Run("非口令键不被误取", func(t *testing.T) {
		// USER=admin 不能被当成口令；真正的口令在后面
		p := write("mixed", "MOCENTER_USER=admin\nMOCENTER_PASSWORD=RealPw\n", 0600)
		got, _ := readPasswordFile(p)
		if got != "RealPw" {
			t.Fatalf("口令 = %q，期望 RealPw（不能把 USER 行当口令）", got)
		}
	})

	t.Run("权限过宽必须拒绝", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows 的权限位语义不同，已在实现里跳过该检查")
		}
		for _, mode := range []os.FileMode{0644, 0640, 0604, 0666} {
			p := write("loose"+mode.String(), "SomePw123\n", mode)
			if _, err := readPasswordFile(p); err == nil {
				t.Fatalf("权限 %04o 的文件居然被接受了（应为拒绝）", mode)
			} else if !strings.Contains(err.Error(), "0600") {
				t.Fatalf("权限 %04o 的报错没提示改成 0600: %v", mode, err)
			}
		}
	})

	t.Run("空文件与仅注释必须拒绝", func(t *testing.T) {
		for name, content := range map[string]string{
			"empty":     "",
			"blank":     "\n\n  \n",
			"comments":  "# 只有注释\n# MOCENTER_PASSWORD=x\n",
			"emptyval":  "MOCENTER_PASSWORD=\n",
		} {
			p := write("bad_"+name, content, 0600)
			if _, err := readPasswordFile(p); err == nil {
				t.Fatalf("%s: 空口令被接受了（应拒绝，否则会建出空密码坏账号）", name)
			}
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		if _, err := readPasswordFile(filepath.Join(dir, "nope")); err == nil {
			t.Fatal("文件不存在时应报错（fail-closed，不能静默回退）")
		}
	})

	t.Run("路径是目录", func(t *testing.T) {
		if _, err := readPasswordFile(dir); err == nil {
			t.Fatal("目录应被拒绝")
		}
	})
}
