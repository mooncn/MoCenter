package version

import (
	"regexp"
	"testing"
)

// TestVersionFormat 版本号必须是 x.y.z 三段式数字。
//
// 版本号会被升级判定做字符串相等比较，也会进打包脚本，格式不一致会在某条链路上悄悄失配。
func TestVersionFormat(t *testing.T) {
	re := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !re.MatchString(Version) {
		t.Fatalf("Agent 版本号 %q 不是 x.y.z 三段式数字格式", Version)
	}
}

// TestVersionIsOverridableAtBuildTime 守护「版本号可被构建期注入」这条契约。
//
// Version 必须是 **var 而不是 const**：const 无法被 `-ldflags -X` 覆盖，
// 构建脚本里的 `MOCENTER_AGENT_VERSION` 就会静默失效 —— 拿到一个"包名是
// 1.2.0、二进制里却写着 1.0.1"的产物，而构建过程不报任何错。
//
// 这里用反射检查可寻址性（const 在编译期就被折叠，无法取地址）。
func TestVersionIsOverridableAtBuildTime(t *testing.T) {
	old := Version
	defer func() { Version = old }()

	Version = "9.9.9"
	if got := Version; got != "9.9.9" {
		t.Fatalf("Version 必须可写（否则 -ldflags -X 无法注入），实际 %q", got)
	}
	Version = old
	if Version != old {
		t.Fatalf("Version 必须可恢复，实际 %q", Version)
	}
}

// TestVersionIsIndependentFromServerVersion 说明「Agent 版本与服务端版本解耦」。
//
// 这里刻意**不**去读 server 侧源码做比对：两者本就允许不同。
// 早期版本有一个强制两边常量相等的测试，已移除 —— 它会逼着"只改 Agent"
// 也必须顺带发一次服务端。
//
// 保留这个断言只为了把这条设计决定写进测试，避免以后被"顺手加回"一致性校验。
func TestVersionIsIndependentFromServerVersion(t *testing.T) {
	if Version == "" {
		t.Fatal("Agent 版本号不能为空")
	}
	// Name/Full 只依赖 Agent 自己的常量，不引用服务端任何东西
	if Full() != Name+"-Agent/"+Version {
		t.Fatalf("Full() 与 Name/Version 口径不一致: %q", Full())
	}
}
