package main

import (
	"flag"
	"testing"
)

// TestNormalizeBlankFlags 钉住「空串参数按未配置处理」这条语义。
//
// 背景（Rocky8 实机抓到的回归）：mocenter-server.service 的 ExecStart 写成
// `-http "${MOCENTER_HTTP}"`，EnvironmentFile 里没配 MOCENTER_HTTP 时进程收到的是
// 一个空 argv 而不是"没有这个参数"，于是 http.Server 按内置默认的 :443 监听（不是 :8080），
// 并且 -http 的"配置文件回并"条件失效。同时 flag.Visit 把它算成显式给出，
// 自动回写会用空串盖掉配置文件里的值。
func TestNormalizeBlankFlags(t *testing.T) {
	newSet := func() (*flag.FlagSet, *string, *string, *string, *int) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		withDefault := fs.String("http", ":8080", "有非空默认值")
		emptyDefault := fs.String("user", "", "默认值本来就是空串")
		excluded := fs.String("vnc-ports", "5900-5999", "空串是有效取值，不参与归一")
		n := fs.Int("hb", 30, "整型不受影响")
		return fs, withDefault, emptyDefault, excluded, n
	}
	value := func(fs *flag.FlagSet, name string) string {
		t.Helper()
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("参数 %s 未定义", name)
		}
		return f.Value.String()
	}

	t.Run("空串显式传入时回落默认值并登记", func(t *testing.T) {
		fs, httpAddr, _, _, _ := newSet()
		if err := fs.Parse([]string{"-http", ""}); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		blank := normalizeBlankFlags(fs, "vnc-ports")
		if !blank["http"] {
			t.Fatal("http 应被登记为「按未配置处理」")
		}
		if got := *httpAddr; got != ":8080" {
			t.Fatalf("http = %q，期望回落成 %q", got, ":8080")
		}
	})

	t.Run("排除的参数保留空串语义", func(t *testing.T) {
		fs, _, _, excluded, _ := newSet()
		if err := fs.Parse([]string{"-vnc-ports", ""}); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		blank := normalizeBlankFlags(fs, "vnc-ports")
		if blank["vnc-ports"] {
			t.Fatal("vnc-ports 的空串是「不限制」这个显式语义，不能被归一")
		}
		if got := *excluded; got != "" {
			t.Fatalf("vnc-ports = %q，期望保持空串", got)
		}
	})

	t.Run("默认值本就是空串时只登记不改值", func(t *testing.T) {
		fs, _, user, _, _ := newSet()
		if err := fs.Parse([]string{"-user", ""}); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		blank := normalizeBlankFlags(fs, "vnc-ports")
		if !blank["user"] {
			t.Fatal("user 仍应被登记：它的空值不该回写进配置文件")
		}
		if got := *user; got != "" {
			t.Fatalf("user = %q，期望空串", got)
		}
	})

	t.Run("非空取值与零值整型不受影响", func(t *testing.T) {
		fs, httpAddr, _, _, n := newSet()
		if err := fs.Parse([]string{"-http", ":18080", "-hb", "0"}); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		blank := normalizeBlankFlags(fs, "vnc-ports")
		if blank["http"] || blank["hb"] {
			t.Fatalf("归一不该碰这些参数：%v", blank)
		}
		if got := *httpAddr; got != ":18080" {
			t.Fatalf("http = %q，期望原样保留", got)
		}
		if *n != 0 {
			t.Fatalf("hb = %d，期望原样保留 0", *n)
		}
	})

	t.Run("清除显式标记只影响被归一的参数", func(t *testing.T) {
		fs, _, _, _, _ := newSet()
		if err := fs.Parse([]string{"-http", "", "-user", "admin"}); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		blank := normalizeBlankFlags(fs, "vnc-ports")
		explicit := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		clearBlankExplicit(explicit, blank)
		if explicit["http"] {
			t.Fatal("http 传的是空串，不能再算「显式给出」")
		}
		if !explicit["user"] {
			t.Fatal("user 显式给了 admin，必须保留显式标记（否则不回写配置）")
		}
		if value(fs, "http") != ":8080" {
			t.Fatalf("http = %q，期望 %q", value(fs, "http"), ":8080")
		}
	})
}
