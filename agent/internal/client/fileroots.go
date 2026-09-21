package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 本文件收口 M-9 中"服务端可以把 Agent 本机的任意路径当读写目标"这一半：
// 文件分发（写）与文件下载（读）两个入口的下发路径都必须落在本机配置的
// 文件根目录之内。
//
// 为什么默认是"沙箱目录"而不是"整台机器"：Agent 通常以 SYSTEM/root 运行，
// 一旦服务端私钥泄露或域名被劫持，无限制的路径下发就等于让对端以最高权限
// 读 /etc/shadow、写 authorized_keys。远程命令执行本身是产品功能、不收口，
// 所以这道闸门的意义是"把最常用的两条投毒/外带通道从默认能力里拿掉"，
// 需要时由运维在本机用 -file-roots 显式扩大范围。

// guardPath 是本机配置下的路径闸门：把服务端下发的路径限制在 cfg.FileRoots 内。
func (c *Client) guardPath(raw string) (string, error) {
	return withinRoots(c.cfg.FileRoots, raw)
}

// FileRoots 返回生效的文件读写根目录（含默认值），供启动日志与自检使用。
func (c *Client) FileRoots() []string {
	out := make([]string, len(c.cfg.FileRoots))
	copy(out, c.cfg.FileRoots)
	return out
}

// DefaultFileRoot 返回未配置 -file-roots 时的默认沙箱目录：<配置目录>/files。
//
// 刻意不用可执行文件目录：升级会把新二进制写到那儿，把同一目录开成可写
// 目标等于让"下发一个同名文件"参与覆盖二进制。
func DefaultFileRoot(configPath string) string {
	dir := "."
	if strings.TrimSpace(configPath) != "" {
		dir = filepath.Dir(configPath)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "files")
}

// withinRoots 把下发路径规范化并确认它落在 roots 之一以内，返回可供 os 系列
// 函数使用的绝对路径（已解析符号链接的部分会保持解析后的形态）。
//
// 三层检查缺一不可，各自挡一种手法：
//   - 只接受绝对路径：相对路径的含义取决于 Agent 的工作目录，服务端猜不到，
//     运维也容易配错根目录；
//   - Clean + Rel：挡掉 `root/../etc/passwd` 这类字符串层面的逃逸；
//   - EvalSymlinks（对最近的存在祖先）：挡掉"根目录里先放一个指向 /etc 的
//     软链接，再往链接里写 shadow"这种字符串干净、实际越界的写法。
func withinRoots(roots []string, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("路径为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("路径含非法字符")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("必须是绝对路径（当前 %q）", raw)
	}
	target, err := resolvePath(raw)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		r, rerr := resolveRoot(root)
		if rerr != nil {
			continue
		}
		ok, cerr := contains(r, target)
		if cerr != nil {
			return "", cerr
		}
		if ok {
			return target, nil
		}
	}
	return "", fmt.Errorf("路径 %q 不在允许的文件根目录内（本机 -file-roots 当前为 %s）",
		raw, strings.Join(quotedList(roots), ", "))
}

// resolveRoot 解析文件根。
//
// 三种状态要区别对待：
//   - 已存在的目录：按符号链接解开后作为基准（否则"根目录是个指向别处的链接"
//     就把整台机器伪装成根目录之下）；
//   - 还不存在的路径：允许，作为被钉住的绝对前缀参与比较 —— 默认沙箱
//     <配置目录>/files 在第一次下发之前就是这种状态，写文件时 MkdirAll 会建出来；
//   - 存在但不是目录：配置写错了，拒绝，不猜运维想写哪个目录。
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		fi, serr := os.Stat(resolved)
		if serr != nil {
			return "", serr
		}
		if !fi.IsDir() {
			return "", errors.New("文件根必须是目录")
		}
		return resolved, nil
	}
	return resolvePath(abs)
}

// resolvePath 取目标路径的规范形态：把"最近的存在祖先"按符号链接解开后再
// 接回尚未创建的部分 —— 新建文件本身不存在，整条路径无法直接 EvalSymlinks。
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	tail := []string{}
	cur := abs
	for {
		if resolved, rerr := filepath.EvalSymlinks(cur); rerr == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// 一路走到文件系统根都不存在（通常是父目录还没建）：按原样比较
			return abs, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// contains 判断 target 是否等于 root 或在其之下（按路径段比较，不做前缀字符串匹配）。
func contains(root, target string) (bool, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false, err
	}
	if filepath.IsAbs(rel) {
		// 跨盘符（Windows 上 C:\ 与 D:\ 之间）：Rel 会给出绝对路径
		return false, nil
	}
	switch rel {
	case "..":
		return false, nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

func quotedList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it) == "" {
			continue
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return []string{"（未配置）"}
	}
	return out
}
