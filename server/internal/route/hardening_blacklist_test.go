package route

import "testing"

// 本文件锚住"黑名单护栏至少拦住**不需要任何技巧**的常见写法"这一性质。
//
// 说清边界：IsBlocked 是纵深防御的界面护栏，**不是安全边界** —— 一个被拦的
// 字面串总能用 shell 语法改写成别的字符串。所以这里刻意**不**断言
// "任意混淆都能拦住"（那种断言会给人假的安心），只断言：
//   1. 显然等价的写法（多空白 / 引号拼接 / 行内注释 / 大小写 / 前导空白）要拦住；
//   2. 无害命令不许误伤（误报会让运维干脆关掉黑名单，比漏报更糟）。

func TestIsBlockedCatchesObviousEquivalents(t *testing.T) {
	r := NewRouter(nil, nil)
	blocked := []string{
		"rm -rf /",
		"rm  -rf  /",          // 多打一个空格
		"\trm\t-rf\t/",        // 制表分隔
		"  rm -rf /",          // 前导空白
		"rm -rf / # 清理根目录",    // 行内注释
		"RM -RF /",            // 大小写
		"r''m -rf /",          // 引号拼接（shell 会吃掉引号）
		`r""m -rf /`,          // 双引号拼接
		"sudo",                // 内置规则
		"mkfs.ext4 /dev/sda1", // 内置规则
	}
	for _, c := range blocked {
		if hit, _ := r.IsBlocked(c); !hit {
			t.Errorf("显然等价的写法未被拦下（护栏形同虚设）：%q", c)
		}
	}
}

func TestIsBlockedDoesNotOverreach(t *testing.T) {
	r := NewRouter(nil, nil)
	allowed := []string{
		"echo hi",
		"ls -la /tmp",
		"df -h",
		"systemctl status nginx",
		"cat /etc/hostname",
		"# 只是注释",
	}
	for _, c := range allowed {
		if hit, why := r.IsBlocked(c); hit {
			t.Errorf("无害命令被误伤（会逼运维关掉黑名单）：%q 命中规则 %q", c, why)
		}
	}
}

// normalizeForBlacklist 本身的性质：只做不改变语义方向的保守归一化。
func TestNormalizeForBlacklistIsConservative(t *testing.T) {
	cases := map[string]string{
		"CP -R  / A":     "cp -r / a",
		"a\tb":           "a b",
		"echo 'x'":       "echo x",
		`echo "x"`:       "echo x",
		"restart # note": "restart",
		// 词首的 # 起注释，其后内容会被截掉 —— 与 shell 一致（`echo a # b` 里 `# b`
		// 本就不参与执行），因此这里期望的是截断后的结果，不是"吞掉了有用内容"。
		"no # mid-token #": "no",
		"":                 "",
	}
	for in, want := range cases {
		if got := normalizeForBlacklist(in); got != want {
			t.Errorf("normalizeForBlacklist(%q) = %q，期望 %q", in, got, want)
		}
	}
}
