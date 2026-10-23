package client

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// 文本预览 / 编辑：读文件时统一转成 UTF-8 给服务端，写回时按指定编码转码。
//
// 编码检测复用 golang.org/x/text（解码器/编码器）而不是自己写码表：
// 检测策略是"BOM → UTF-8 合法性 → GB18030/Big5/Shift_JIS 往返校验 → Windows-1252"。
// 往返校验（解码后再编码必须与原字节一致）能挡掉大部分"用错编码也能解出乱码"的情况；
// 但 GBK/Big5 的字节空间有重叠，界面仍提供手动指定编码的入口。
const (
	textDefaultLimit = 10 << 20
	// textHardLimit Agent 侧硬上限：即使服务端配置成更大的值也不越过，
	// 避免一个被攻破/配错的服务端让 Agent 把上 GB 的文件读进内存。
	textHardLimit = 64 << 20
	// textDetectSample 只在文件头部做检测：整份文件反复解码在 10MB 级会明显拖慢。
	textDetectSample = 64 << 10
)

var (
	errBinaryFile = errors.New("看起来是二进制文件，无法按文本打开")
	utf8BOMBytes  = []byte{0xEF, 0xBB, 0xBF}
)

// textEncodingLabels 是界面可选的编码（协议里用同一组字符串）。
var textEncodingLabels = []string{
	"UTF-8", "UTF-8 BOM", "UTF-16LE", "UTF-16BE",
	"GB18030", "GBK", "Big5", "Shift_JIS", "Windows-1252", "ISO-8859-1",
}

// textEncoding 把标签映射到 x/text 编码器；别名一并接受，方便手写 API 调用。
func textEncoding(label string) (encoding.Encoding, bool) {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "UTF-8", "UTF8":
		return unicode.UTF8, true
	case "UTF-8 BOM", "UTF8 BOM":
		return unicode.UTF8, true // BOM 由 decode/encodeText 自己加减
	case "UTF-16LE":
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), true
	case "UTF-16BE":
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), true
	case "GB18030":
		return simplifiedchinese.GB18030, true
	case "GBK", "GB2312":
		return simplifiedchinese.GBK, true
	case "BIG5":
		return traditionalchinese.Big5, true
	case "SHIFT_JIS", "SHIFT-JIS", "SJIS":
		return japanese.ShiftJIS, true
	case "WINDOWS-1252", "CP1252":
		return charmap.Windows1252, true
	case "ISO-8859-1", "LATIN1":
		return charmap.ISO8859_1, true
	}
	return nil, false
}

// textLimit 把服务端下发的上限夹到 [默认值, 硬上限]。
func textLimit(v int64) int64 {
	if v <= 0 {
		return textDefaultLimit
	}
	if v > textHardLimit {
		return textHardLimit
	}
	return v
}

func fmtByteSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// detectTextEncoding 返回检测到的编码；binary=true 表示不该按文本打开。
func detectTextEncoding(data []byte) (label string, binary bool) {
	switch {
	case bytes.HasPrefix(data, utf8BOMBytes):
		return "UTF-8 BOM", false
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return "UTF-16LE", false
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return "UTF-16BE", false
	}
	sample := data
	if len(sample) > textDetectSample {
		sample = sample[:textDetectSample]
	}
	if looksBinary(sample) {
		return "", true
	}
	if utf8PrefixValid(sample) {
		return "UTF-8", false
	}
	// 顺序有讲究：先中文编码再日文，最后才是"什么字节都能解"的 Windows-1252。
	for _, cand := range []string{"GB18030", "Big5", "Shift_JIS", "Windows-1252"} {
		if roundTripOK(sample, cand) {
			return cand, false
		}
	}
	// 都不成立时按中文环境最常见的 GB18030 兜底，由用户手动纠正。
	return "GB18030", false
}

// looksBinary 用"NUL 与控制字符比例"粗略判断二进制：
// 文本里出现 NUL 基本就是二进制；控制字符（除 \t\n\v\f\r 与 ESC）超过 5% 同理。
// ANSI 彩色日志里的 ESC 不算控制字符，否则 tail 一份日志就会被误判。
func looksBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	nul, ctrl := 0, 0
	for _, c := range b {
		switch {
		case c == 0:
			nul++
		case c < 0x09, c > 0x0D && c < 0x20 && c != 0x1B, c == 0x7F:
			ctrl++
		}
	}
	return nul > 0 || ctrl*100/len(b) > 5
}

// utf8PrefixValid 允许样本恰好在多字节字符中间被截断（最多回退 3 个字节）。
// 空文件也算 UTF-8：否则会一路掉到"什么都能解"的 GB18030 兜底上。
func utf8PrefixValid(b []byte) bool {
	if utf8.Valid(b) {
		return true
	}
	for i := 0; i < 3 && len(b) > 0; i++ {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return true
		}
	}
	return false
}

// roundTripOK 用候选编码解码再编码，必须与原字节完全一致且不出现替换字符。
func roundTripOK(sample []byte, label string) bool {
	enc, ok := textEncoding(label)
	if !ok {
		return false
	}
	s := sample
	// 截断在字符中间会让解码报错：最多回退 3 个字节再试。
	for i := 0; i < 4 && len(s) > 0; i++ {
		decoded, _, err := transform.Bytes(enc.NewDecoder(), s)
		if err == nil && !bytes.ContainsRune(decoded, utf8.RuneError) {
			back, _, err2 := transform.Bytes(enc.NewEncoder(), decoded)
			if err2 == nil && bytes.Equal(back, s) {
				return true
			}
		}
		s = s[:len(s)-1]
	}
	return false
}

// decodeText 按标签把文件字节转成 UTF-8 字符串。
// 显式指定编码时不做检测：用户的选择优先，解出替换字符就报错提示换一种。
func decodeText(data []byte, label string) (string, string, error) {
	up := strings.ToUpper(strings.TrimSpace(label))
	switch up {
	case "UTF-8", "UTF8", "UTF-8 BOM", "UTF8 BOM":
		body := bytes.TrimPrefix(data, utf8BOMBytes)
		if !utf8.Valid(body) {
			return "", "", errors.New("不是合法的 UTF-8 文本，请换一种编码打开")
		}
		// 保存时沿用用户选的标签：选 "UTF-8 BOM" 才会写回 BOM，
		// 不因为原文件恰好带 BOM 就悄悄改变用户的编码选择。
		used := "UTF-8"
		if strings.Contains(up, "BOM") {
			used = "UTF-8 BOM"
		}
		return string(body), used, nil
	case "UTF-16LE", "UTF-16BE":
		order := unicode.LittleEndian
		if up == "UTF-16BE" {
			order = unicode.BigEndian
		}
		body := data
		if len(body) >= 2 {
			le := body[0] == 0xFF && body[1] == 0xFE
			be := body[0] == 0xFE && body[1] == 0xFF
			if (order == unicode.LittleEndian && le) || (order == unicode.BigEndian && be) {
				body = body[2:]
			}
		}
		out, _, err := transform.String(unicode.UTF16(order, unicode.IgnoreBOM).NewDecoder(), string(body))
		if err != nil {
			return "", "", fmt.Errorf("按 %s 解码失败: %v", up, err)
		}
		if strings.ContainsRune(out, utf8.RuneError) {
			return "", "", fmt.Errorf("按 %s 解码出现非法字符，可能选错了编码", up)
		}
		return out, up, nil
	}
	enc, ok := textEncoding(label)
	if !ok {
		return "", "", fmt.Errorf("不支持的编码: %s", label)
	}
	out, _, err := transform.String(enc.NewDecoder(), string(data))
	if err != nil {
		return "", "", fmt.Errorf("按 %s 解码失败: %v", label, err)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		return "", "", fmt.Errorf("按 %s 解码出现非法字符，可能选错了编码", label)
	}
	return out, strings.ToUpper(strings.TrimSpace(label)), nil
}

// encodeText 把 UTF-8 内容按目标编码转成字节（UTF-16 与 UTF-8 BOM 会补 BOM）。
func encodeText(content, label string) ([]byte, error) {
	up := strings.ToUpper(strings.TrimSpace(label))
	switch up {
	case "", "UTF-8", "UTF8":
		return []byte(content), nil
	case "UTF-8 BOM", "UTF8 BOM":
		return append(append([]byte{}, utf8BOMBytes...), []byte(content)...), nil
	case "UTF-16LE", "UTF-16BE":
		order := unicode.LittleEndian
		bom := []byte{0xFF, 0xFE}
		if up == "UTF-16BE" {
			order = unicode.BigEndian
			bom = []byte{0xFE, 0xFF}
		}
		body, _, err := transform.Bytes(unicode.UTF16(order, unicode.IgnoreBOM).NewEncoder(), []byte(content))
		if err != nil {
			return nil, fmt.Errorf("按 %s 编码失败: %v", up, err)
		}
		return append(bom, body...), nil
	}
	enc, ok := textEncoding(label)
	if !ok {
		return nil, fmt.Errorf("不支持的编码: %s", label)
	}
	out, _, err := transform.Bytes(enc.NewEncoder(), []byte(content))
	if err != nil {
		return nil, fmt.Errorf("按 %s 编码失败（内容里有该编码不支持的字符）: %v", label, err)
	}
	return out, nil
}

// readTextFile 读取并按编码转成 UTF-8；超过上限或看起来是二进制时返回错误。
func readTextFile(path, encodingLabel string, limit int64) (content, used, detected string, size, modTime int64, err error) {
	st, serr := os.Stat(path) // 跟随符号链接：编辑链接指向的文件是合理操作
	if serr != nil {
		return "", "", "", 0, 0, fmt.Errorf("读取文件失败: %v", serr)
	}
	if st.IsDir() {
		return "", "", "", 0, 0, errors.New("这是一个目录，不能按文本打开")
	}
	if st.Size() > limit {
		return "", "", "", 0, 0, fmt.Errorf("文件大小 %s 超过在线编辑上限 %s", fmtByteSize(st.Size()), fmtByteSize(limit))
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", "", "", 0, 0, fmt.Errorf("读取文件失败: %v", rerr)
	}
	if int64(len(data)) > limit {
		return "", "", "", 0, 0, fmt.Errorf("文件大小 %s 超过在线编辑上限 %s", fmtByteSize(int64(len(data))), fmtByteSize(limit))
	}

	detected, binary := detectTextEncoding(data)
	if binary {
		return "", "", "", 0, 0, errBinaryFile
	}
	used = strings.TrimSpace(encodingLabel)
	if used == "" {
		used = detected
	}
	content, used, derr := decodeText(data, used)
	if derr != nil {
		return "", "", "", 0, 0, derr
	}
	return content, used, detected, int64(len(data)), st.ModTime().Unix(), nil
}

// writeTextFile 按目标编码写回。返回写入的字节数与实际使用的编码。
//
// 先写同目录临时文件再 rename：中途失败不会把原文件截成半截。
// 目标是符号链接时改为写穿（否则 rename 会把链接本身替换成普通文件）。
func writeTextFile(path, content, encodingLabel string, limit int64) (int64, string, error) {
	if strings.TrimSpace(encodingLabel) == "" {
		encodingLabel = "UTF-8"
	}
	if _, ok := textEncoding(encodingLabel); !ok {
		return 0, "", fmt.Errorf("不支持的编码: %s", encodingLabel)
	}
	data, err := encodeText(content, encodingLabel)
	if err != nil {
		return 0, "", err
	}
	if int64(len(data)) > limit {
		return 0, "", fmt.Errorf("保存后文件为 %s，超过在线编辑上限 %s", fmtByteSize(int64(len(data))), fmtByteSize(limit))
	}

	mode := os.FileMode(0o644)
	if st, serr := os.Stat(path); serr == nil {
		if st.IsDir() {
			return 0, "", errors.New("这是一个目录，不能保存为文件")
		}
		mode = st.Mode().Perm()
	}
	if li, lerr := os.Lstat(path); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
		if werr := os.WriteFile(path, data, mode); werr != nil {
			return 0, "", fmt.Errorf("写入失败: %v", werr)
		}
		return int64(len(data)), strings.ToUpper(strings.TrimSpace(encodingLabel)), nil
	}

	tmp, terr := os.CreateTemp(filepath.Dir(path), ".mc-edit-*")
	if terr != nil {
		return 0, "", fmt.Errorf("创建临时文件失败: %v", terr)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		return 0, "", fmt.Errorf("写入失败: %v", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return 0, "", fmt.Errorf("写入失败: %v", cerr)
	}
	_ = os.Chmod(tmpName, mode)
	if rerr := os.Rename(tmpName, path); rerr != nil {
		return 0, "", fmt.Errorf("写入失败: %v", rerr)
	}
	return int64(len(data)), strings.ToUpper(strings.TrimSpace(encodingLabel)), nil
}
