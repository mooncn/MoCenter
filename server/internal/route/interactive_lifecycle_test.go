package route

import (
	"fmt"
	"testing"
)

// 本文件锚住 M-N2 的两个性质：
//   1. 交互会话结束时缓冲必须被回收（DropInteractive 不再是死代码）；
//   2. 触顶时的兜底是**淘汰最旧**，而不是整表清空 —— 整表清空会连带丢掉
//      当时活跃终端的半行输入，表现为"别的窗口打着字突然半行错乱"。

func TestDropInteractiveReleasesBuffer(t *testing.T) {
	r := NewRouter(nil, nil)
	key := InteractiveKey("agent-1", "cmd-1")

	// 先喂半行（不带换行），缓冲里应有一个键
	if _, blocked := r.FilterInteractive(key, "rm -rf"); blocked != "" {
		t.Fatal("半行输入不该被拦")
	}
	if got := r.interactiveBufs(); got != 1 {
		t.Fatalf("喂入后应有 1 个缓冲，实际 %d", got)
	}

	r.DropInteractive(key)
	if got := r.interactiveBufs(); got != 0 {
		t.Fatalf("DropInteractive 后缓冲未回收（半行命令会驻留到进程退出）：%d", got)
	}
	_ = r.iguardKeep() // 确保 guard 已初始化，便于后面的断言

	// 回收后再喂同一键应重新建缓冲，且半行状态是干净的：
	// 单独喂一个 "/" 不该命中（旧半行若残留，拼接后会成为 "rm -rf/" 之外的东西，
	// 关键判据是下面补成完整危险行时才拦）。
	if _, blocked := r.FilterInteractive(key, "/"); blocked != "" {
		t.Fatal("回收后的新输入不该命中旧半行")
	}
	// 补齐成完整危险行：`rm -rf /`（先 rm -rf，再 /）
	if _, blocked := r.FilterInteractive(key, "rm -rf "); blocked != "" {
		t.Fatal("未到行末不该拦")
	}
	if _, blocked := r.FilterInteractive(key, "/\nthis should be eaten by the newline check"); blocked == "" {
		t.Fatal("完整的危险行应被拦住（说明半行状态确实重建了）")
	}
}

func TestDropInteractiveIgnoresEmptyKey(t *testing.T) {
	r := NewRouter(nil, nil)
	r.DropInteractive("") // 不该 panic
}

func TestInteractiveGuardEvictsOldestNotWholeTable(t *testing.T) {
	r := NewRouter(nil, nil)
	keep := r.iguardKeep()

	// 灌满 keep 个会话，各自留半行
	for i := 0; i < keep; i++ {
		k := InteractiveKey(fmt.Sprintf("agent-%d", i), "cmd")
		if _, b := r.FilterInteractive(k, "half-line"); b != "" {
			t.Fatal("半行不该被拦")
		}
	}
	if got := r.interactiveBufs(); got != keep {
		t.Fatalf("应恰好 %d 个缓冲，实际 %d", keep, got)
	}

	// 再开一个新会话：只应淘汰最旧的一个，其余（含它们各自的半行）必须保留
	newKey := InteractiveKey("agent-new", "cmd")
	if _, b := r.FilterInteractive(newKey, "x"); b != "" {
		t.Fatal("新会话输入不该被拦")
	}
	if got := r.interactiveBufs(); got != keep {
		t.Fatalf("触顶后缓冲数应先淘汰再新增（保持 %d），实际 %d —— 整表清空会让活跃终端半行错乱", keep, got)
	}
	// 最旧的键应已被淘汰
	if _, b := r.FilterInteractive(InteractiveKey("agent-0", "cmd"), "y"); b != "" {
		t.Fatal("不该被拦")
	}
	// 最新的键仍在（其半行状态没被清掉）
	if _, b := r.FilterInteractive(newKey, "z"); b != "" {
		t.Fatal("不该被拦")
	}
}

// 交互模式下的黑名单检查同样受益于归一化（IsBlocked），这里锚住端到端行为。
func TestFilterInteractiveBlocksObfuscatedLine(t *testing.T) {
	r := NewRouter(nil, nil)
	key := InteractiveKey("a", "c")
	// 逐字符喂入 `rm  -rf /` + 回车：多打的空格不该成为绕过手段
	for _, ch := range "rm  -rf /" {
		if _, b := r.FilterInteractive(key, string(ch)); b != "" {
			t.Fatalf("未到行末不该拦：%q", b)
		}
	}
	if _, b := r.FilterInteractive(key, "\n"); b == "" {
		t.Fatal("多空格的 rm -rf / 在交互模式下未被拦下")
	}
}
