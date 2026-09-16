package version

import (
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

// TestVersionIsIndependentFromAgent 说明并守护「服务端版本号与 Agent 版本号解耦」。
//
// 这里原本有一个 TestAgentVersionMatches，会读取 agent 侧源码强制两边常量相等。
// 该约束已移除：Agent 版本表达的是"Agent 功能迭代到哪一代"，由 Agent 自己的
// 发布节奏决定；服务端只把它当展示字段，不做任何一致性校验，
// 也不应因为 Agent 版本高低而拒绝其接入或升级。
//
// 因此这里不再去读 agent 源码，只验证服务端自身的版本号口径合法
// （它仍会进入备份清单、/api/version 等对外输出）。
func TestVersionIsIndependentFromAgent(t *testing.T) {
	if Version == "" {
		t.Fatal("服务端版本号不能为空")
	}
	if Full() != Name+"/"+Version {
		t.Fatalf("Full() 与 Name/Version 口径不一致")
	}
}
