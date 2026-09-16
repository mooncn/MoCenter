package version

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestVersionFormat 版本号必须是 x.y.z 三段式数字。
//
// 为什么要卡格式：版本号会被用于升级判定（字符串相等比较）与打包脚本
// （MOCENTER_VERSION / .iss AppVersion / .deb Version），不一致的格式
// （比如带 -beta 后缀）会在某一条链路上悄悄失配。
func TestVersionFormat(t *testing.T) {
	re := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !re.MatchString(Version) {
		t.Fatalf("版本号 %q 不是 x.y.z 三段式数字格式", Version)
	}
}

// TestAgentVersionMatches 守护「服务端与 Agent 版本号一致」这条发布纪律。
//
// 两个包无法互相 import（server/agent 是两个独立 module），因此这里
// 直接读 agent 侧源码比对常量值 —— 一旦有人只改了一边，测试立刻失败。
// 这比注释约定可靠：注释不会拦住 CI。
func TestAgentVersionMatches(t *testing.T) {
	p := filepath.Join("..", "..", "..", "agent", "internal", "version", "version.go")
	b, err := os.ReadFile(p)
	if err != nil {
		// 在只拷贝 server 目录的场景（例如按模块发布）下跳过，而不是误报失败
		t.Skipf("无法读取 agent 侧版本文件（%v），跳过一致性校验", err)
	}
	re := regexp.MustCompile(`(?m)^\s*const\s+Version\s*=\s*"([^"]+)"`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("在 %s 中未找到 Version 常量", p)
	}
	if got := string(m[1]); got != Version {
		t.Fatalf("服务端与 Agent 版本号不一致：server=%q agent=%q（发版时必须同时修改两处）", Version, got)
	}
}

func TestFull(t *testing.T) {
	if got := Full(); got != Name+"/"+Version {
		t.Fatalf("Full() = %q", got)
	}
}
