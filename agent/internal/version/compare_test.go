package version

import (
	"errors"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.10.0", "1.9.0", 1},    // 数值比较，不是字典序
		{"v1.2.3", "1.2.3", 0},    // 容忍 v 前缀
		{"1.2", "1.2.0", 0},       // 缺省尾段按 0
		{"1.2.3-dev", "1.2.3", 0}, /* 构建标记不参与大小 */
		{"1.2.3+build9", "1.2.4", -1},
		{"1.0.0.5", "1.0.0.4", 1},
		{" 1.2.3 ", "1.2.3", 0},
	}
	for _, tc := range cases {
		got, err := Compare(tc.a, tc.b)
		if err != nil {
			t.Errorf("Compare(%q,%q) 报错: %v", tc.a, tc.b, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Compare(%q,%q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestCompareRejectsUnparsable 解析不出来时必须报错，而不是"当作相等"。
//
// 升级判定依赖这个错误：判不出升降级还放行，等于给"用合法旧包钉住机群"
// 留了一条"版本号写个 latest 就好"的后门。
func TestCompareRejectsUnparsable(t *testing.T) {
	for _, s := range []string{"", "   ", "latest", "x.y.z", "abc", "."} {
		if _, err := Compare(s, "1.0.0"); !errors.Is(err, ErrUnparsable) {
			t.Errorf("Compare(%q,…) 应返回 ErrUnparsable，实际 %v", s, err)
		}
		if _, err := Compare("1.0.0", s); !errors.Is(err, ErrUnparsable) {
			t.Errorf("Compare(…,%q) 应返回 ErrUnparsable，实际 %v", s, err)
		}
	}
}

// TestCompareSymmetry 反向调用必须给出相反数：升级判定两边都用得到。
func TestCompareSymmetry(t *testing.T) {
	for _, pair := range [][2]string{{"1.2.3", "1.2.4"}, {"2.0", "1.9.9"}, {"1.0.0", "1.0.0+build"}} {
		x, err := Compare(pair[0], pair[1])
		if err != nil {
			t.Fatalf("%v", err)
		}
		y, err := Compare(pair[1], pair[0])
		if err != nil {
			t.Fatalf("%v", err)
		}
		if x != -y {
			t.Fatalf("Compare(%q,%q)=%d 而反向=%d", pair[0], pair[1], x, y)
		}
	}
}
