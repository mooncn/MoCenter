package client

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDetectTextEncoding 编码检测：BOM、UTF-8、GB18030、UTF-16、二进制。
func TestDetectTextEncoding(t *testing.T) {
	gbk, err := encodeText("你好，世界。这是一段中文日志。", "GBK")
	if err != nil {
		t.Fatalf("GBK 编码失败: %v", err)
	}
	cases := []struct {
		name   string
		data   []byte
		want   string
		binary bool
	}{
		{"ascii", []byte("hello world\n"), "UTF-8", false},
		{"utf8-cn", []byte("你好，世界\n"), "UTF-8", false},
		{"utf8-bom", append([]byte{0xEF, 0xBB, 0xBF}, []byte("你好\n")...), "UTF-8 BOM", false},
		{"gbk", gbk, "GB18030", false},
		{"binary", []byte{0x4D, 0x5A, 0x00, 0x00, 0x03}, "", true},
	}
	for _, c := range cases {
		got, binary := detectTextEncoding(c.data)
		if binary != c.binary || (!binary && got != c.want) {
			t.Errorf("%s: got %q binary=%v, want %q binary=%v", c.name, got, binary, c.want, c.binary)
		}
	}

	utf16le, err := encodeText("你好", "UTF-16LE")
	if err != nil {
		t.Fatal(err)
	}
	if got, binary := detectTextEncoding(utf16le); binary || got != "UTF-16LE" {
		t.Errorf("UTF-16LE 检测: got %q binary=%v", got, binary)
	}
}

// TestTextDecodeEncodeRoundTrip 各编码写入后按同编码读回，内容不变。
func TestTextDecodeEncodeRoundTrip(t *testing.T) {
	text := "第一行：配置项=值\nsecond line\ttab\n"
	for _, label := range []string{"UTF-8", "UTF-8 BOM", "UTF-16LE", "UTF-16BE", "GB18030", "GBK"} {
		data, err := encodeText(text, label)
		if err != nil {
			t.Fatalf("%s 编码失败: %v", label, err)
		}
		got, used, err := decodeText(data, label)
		if err != nil {
			t.Fatalf("%s 解码失败: %v", label, err)
		}
		if got != text {
			t.Errorf("%s 往返内容不一致: %q", label, got)
		}
		if used == "" {
			t.Errorf("%s 未返回实际编码", label)
		}
	}
	// 编码器不支持的字符必须报错，而不是静默变成问号
	if _, err := encodeText("emoji 😀", "GBK"); err == nil {
		t.Error("GBK 编码 emoji 应报错")
	}
	// UTF-16 写回带 BOM
	b, _ := encodeText("hi", "UTF-16LE")
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Errorf("UTF-16LE 应带 BOM: % x", b[:2])
	}
}

// TestReadWriteTextFile 文件级读写：编码、大小上限、二进制拒绝。
func TestReadWriteTextFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	if err := os.WriteFile(p, []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	content, used, detected, size, _, err := readTextFile(p, "", textDefaultLimit)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if content != "line1\nline2\n" || used != "UTF-8" || detected != "UTF-8" || size != 12 {
		t.Fatalf("读取结果不正确: content=%q used=%q detected=%q size=%d", content, used, detected, size)
	}

	// 写成 GBK，再按自动检测读回
	if _, _, err := writeTextFile(p, "中文内容\n", "GBK", textDefaultLimit); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	content, used, detected, _, _, err = readTextFile(p, "", textDefaultLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if content != "中文内容\n" || used != "GB18030" || detected != "GB18030" {
		t.Fatalf("GBK 往返不一致: content=%q used=%q detected=%q", content, used, detected)
	}
	// 权限位沿用原文件（Windows 不记录 Unix 权限位，跳过）
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("写入后权限被改成了 %o", st.Mode().Perm())
		}
	}

	// 超过上限
	if _, _, _, _, _, err := readTextFile(p, "", 4); err == nil || !strings.Contains(err.Error(), "超过在线编辑上限") {
		t.Errorf("超限读取应报错: %v", err)
	}
	if _, _, err := writeTextFile(p, strings.Repeat("x", 64), "UTF-8", 16); err == nil || !strings.Contains(err.Error(), "超过在线编辑上限") {
		t.Errorf("超限写入应报错: %v", err)
	}

	// 二进制拒绝
	bin := filepath.Join(dir, "a.bin")
	_ = os.WriteFile(bin, []byte{0x00, 0x01, 0x02}, 0o644)
	if _, _, _, _, _, err := readTextFile(bin, "", textDefaultLimit); err == nil {
		t.Error("二进制文件应拒绝")
	}

	// 目录不能当文本打开
	if _, _, _, _, _, err := readTextFile(dir, "", textDefaultLimit); err == nil {
		t.Error("目录应拒绝")
	}
}
