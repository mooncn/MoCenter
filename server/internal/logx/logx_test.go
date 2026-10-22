package logx

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
)

// capture 把标准库 log 的输出接到内存里。
// 日志器刻意复用标准库 logger（服务模式靠它重定向到文件），所以这里也接它的输出。
func capture(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want Level
		ok   bool
	}{
		{"", Error, true},
		{"error", Error, true},
		{"ERROR", Error, true},
		{" error ", Error, true},
		{"off", Off, true},
		{"none", Off, true},
		{"warn", Warn, true},
		{"warning", Warn, true},
		{"info", Info, true},
		{"debug", Info, true},
		{"trace", Error, false},
		{"verbose", Error, false},
	}
	for _, c := range cases {
		got, ok := ParseLevel(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseLevel(%q) = (%v,%v)，期望 (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{Off: "off", Error: "error", Warn: "warn", Info: "info", Level(99): "error"} {
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q，期望 %q", l, got, want)
		}
	}
}

// TestLevelFiltering 守护四个级别的过滤矩阵：级别越高，输出越少。
func TestLevelFiltering(t *testing.T) {
	cases := []struct {
		level                 Level
		info, warn, err, crit bool
	}{
		{Off, false, false, false, false},
		{Error, false, false, true, true},
		{Warn, false, true, true, true},
		{Info, true, true, true, true},
	}
	for _, c := range cases {
		out := capture(t, func() {
			SetLevel(c.level)
			Infof("i")
			Warnf("w")
			Errorf("e")
			Criticalf("c")
		})
		check := func(tag string, want bool) {
			got := strings.Contains(out, "["+tag+"]")
			if got != want {
				t.Errorf("级别 %v 下 %s 输出=%v，期望 %v（完整输出 %q）", c.level, tag, got, want, out)
			}
		}
		check("INFO", c.info)
		check("WARN", c.warn)
		check("ERROR", c.err)
		check("CRITICAL", c.crit)
	}
}

// TestFatalfIgnoresLevel off 下致命错误仍必须输出——否则服务起不来时零线索。
func TestFatalfIgnoresLevel(t *testing.T) {
	exited := 0
	oldExit := exitFn
	exitFn = func(code int) { exited = code }
	defer func() { exitFn = oldExit }()

	out := capture(t, func() {
		SetLevel(Off)
		Fatalf("配置解析失败: %v", "boom")
	})
	if !strings.Contains(out, "[CRITICAL]") || !strings.Contains(out, "配置解析失败: boom") {
		t.Fatalf("off 下 Fatalf 必须输出，实际 %q", out)
	}
	if exited != 1 {
		t.Fatalf("Fatalf 退出码应为 1，实际 %d", exited)
	}
}

// TestConcurrentUse SetLevel 与各级别输出可以并发调用（竞态用 -race 抓）。
func TestConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetLevel(Level((i + j) % 4))
				Infof("i")
				Warnf("w")
				Errorf("e")
				_ = Current().String()
			}
		}(i)
	}
	wg.Wait()
	SetLevel(Error)
}
