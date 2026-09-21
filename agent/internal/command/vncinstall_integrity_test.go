package command

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 钉扎表必须覆盖每一个「会被执行或解压」的安装包名。
//
// 少一条就等于该包退化成「只验魔数」—— 而这条链的产物是以 SYSTEM 执行的安装器、
// 以及 pnputil 加载的内核驱动，正是必须钉住的那类。
func TestInstallPinsCoverAllPackages(t *testing.T) {
	packages := []string{uvncSetupX64Name, uvncSetupX86Name, vddZipName}
	for _, name := range packages {
		digest := pinnedDigest(name)
		if digest == "" {
			t.Errorf("包 %s 缺少内置钉扎摘要", name)
			continue
		}
		if len(digest) != 64 || strings.ToLower(digest) != digest {
			t.Errorf("包 %s 的钉扎摘要应是 64 位小写十六进制: %q", name, digest)
			continue
		}
		if _, err := hex.DecodeString(digest); err != nil {
			t.Errorf("包 %s 的钉扎摘要不是合法十六进制: %v", name, err)
		}
	}
	// 反向也查：改版本时只更新包名常量、忘了删旧条目，会留下「以为还钉着」的空规则。
	for name := range installPins {
		found := false
		for _, p := range packages {
			if p == name {
				found = true
			}
		}
		if !found {
			t.Errorf("installPins 中存在未知包名 %q（包已改名或已移除，请同步清理）", name)
		}
	}
}

// 执行/解压前的摘要闸：不符必须拒绝，且报错要带上实际摘要，
// 否则运维只能自己再算一遍才知道该把什么写进 installPins。
func TestVerifyDigestGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.bin")
	if err := os.WriteFile(path, []byte("mocenter package payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := verifyDigest(path, digest, "pkg.bin"); err != nil {
		t.Errorf("摘要一致应通过: %v", err)
	}
	// 厂商清单常以大写 HEX 发布（winget 就是），大小写不该影响判定
	if err := verifyDigest(path, strings.ToUpper(digest), "pkg.bin"); err != nil {
		t.Errorf("大写摘要应同样通过: %v", err)
	}
	if err := verifyDigest(path, "", "pkg.bin"); err != nil {
		t.Errorf("期望摘要为空表示不校验，不应报错: %v", err)
	}

	err = verifyDigest(path, strings.Repeat("0", 64), "pkg.bin")
	if err == nil {
		t.Fatal("摘要不符必须拒绝")
	}
	if !strings.Contains(err.Error(), digestPrefix(digest)) {
		t.Errorf("错误消息应带上实际摘要供运维核对: %v", err)
	}
	if !strings.Contains(err.Error(), "installPins") {
		t.Errorf("错误消息应指明去哪儿更新钉扎: %v", err)
	}
	// 异常短的期望值不能把错误消息截崩
	if err := verifyDigest(path, "abc", "pkg.bin"); err == nil {
		t.Error("过短的期望摘要也应判为不符")
	}
	if err := verifyDigest(filepath.Join(dir, "not-exist"), digest, "not-exist"); err == nil {
		t.Error("文件不存在时应报错而不是放行")
	}
}

// 加速代理只认 https://：明文代理能让任意中间人替换安装包，等同于没有完整性校验。
func TestHTTPProxyOnlyForInstallChain(t *testing.T) {
	if got := httpsProxyOrEmpty("", "t"); got != "" {
		t.Errorf("空代理应返回空串: %q", got)
	}
	for _, bad := range []string{
		"http://fa.example.win/?url=",
		"HTTP://mirror.example.com/?url=",
		"ftp://mirror.example.com/",
		"mirror.example.com/?url=",
		"//mirror.example.com/?url=",
	} {
		if got := httpsProxyOrEmpty(bad, "t"); got != "" {
			t.Errorf("非 https 代理应被忽略: %q -> %q", bad, got)
		}
	}
	const good = "https://mirror.example.com/?url="
	if got := httpsProxyOrEmpty("  "+good+"  ", "t"); got != good {
		t.Errorf("https 代理应原样放行: %q", got)
	}
	// 通过闸门后才会进 proxyURL；确认前缀仍是 https，且原始地址被完整带在 query 里
	const origin = "https://uvnc.eu/download/1800/a.exe"
	if merged := proxyURL(good, origin); !strings.HasPrefix(merged, good) || !strings.Contains(merged, "uvnc.eu") {
		t.Errorf("proxyURL 拼接结果不符合预期: %q", merged)
	}
}

// psQuote：URL 有一部分来自配置（-proxy-ip / 镜像地址），单引号不转义就等于
// 把引号闭合权交给配置值 —— 这些命令跑在 SYSTEM 上下文里。
func TestPSQuoteEscapesSingleQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Windows\Temp\a.exe`, `'C:\Windows\Temp\a.exe'`},
		{`it's`, `'it''s'`},
		{`https://p/?url=x'; Stop-Process`, `'https://p/?url=x''; Stop-Process'`},
		{``, `''`},
	}
	for _, c := range cases {
		if got := psQuote(c.in); got != c.want {
			t.Errorf("psQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 公网取包默认关闭：提示里必须给出「怎么解锁」以及它只能本机设置。
func TestPublicDownloadHintReflectsSwitch(t *testing.T) {
	if hint := publicDownloadHint(VncConfig{}); hint == "" {
		t.Error("默认应给出未解锁说明")
	} else if !strings.Contains(hint, "allow_public_package_download") {
		t.Errorf("提示应指明开关名: %q", hint)
	}
	if hint := publicDownloadHint(VncConfig{AllowPublicDownload: true}); hint != "" {
		t.Errorf("已解锁时不应再提示: %q", hint)
	}
}
