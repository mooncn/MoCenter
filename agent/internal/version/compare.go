package version

import (
	"errors"
	"strconv"
	"strings"
)

// ErrUnparsable 版本号无法解析成可比较的数值序列。
var ErrUnparsable = errors.New("版本号无法解析")

// Compare 按数值逐段比较两个版本号：a<b 返回 -1，a==b 返回 0，a>b 返回 1。
//
// 容忍这些写法：前缀 v（v1.2.3）、缺省尾部段（1.0 等同 1.0.0）、
// 尾随的构建标记（1.2.3-dev、1.2.3+build9 都按 1.2.3 比较）。
// 之所以只比数值段：ldflags 注入的版本号形态五花八门，
// 若把 "-dev" 当成比 "1.2.3" 更大/更小，降级判定就会给出反直觉的答案。
//
// 任一操作数解析不出首个数值段时返回 ErrUnparsable —— 调用方（升级判定）
// 在这种情况下必须**拒绝**而不是放行：判不出升降级的升级指令，
// 正好是"把机群钉在某个旧包上"这类攻击最可能钻的空子。
func Compare(a, b string) (int, error) {
	pa, err := parse(a)
	if err != nil {
		return 0, err
	}
	pb, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
		}
	}
	return 0, nil
}

// parse 取出版本号里的数值段（最多 4 段，够用且避免畸形长串吃内存）。
func parse(s string) ([]int, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return nil, ErrUnparsable
	}
	// 剥掉构建/预发布标记：-dev、+build、_rc1 之类不参与大小比较
	if i := strings.IndexAny(s, "-+_"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		parts = parts[:4]
	}
	out := make([]int, 0, len(parts))
	for idx, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			if idx == 0 {
				return nil, ErrUnparsable
			}
			// 首段之后的空/非数字段按 0 处理（"1.2.x" 视作 1.2.0）
			n = 0
		}
		out = append(out, n)
	}
	return out, nil
}
