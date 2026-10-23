package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"mocenter/agent/internal/logx"
	"mocenter/agent/internal/protocol"
)

// 文件管理：整机文件操作。
//
// 与文件分发/下载通道不同，这里**不再受 -file-roots 沙箱限制**：
// 文件管理定位为管理员对整台主机的文件运维（等价于本产品已有的远程命令能力）。
// 安全性由"仅管理员可调用的认证通道 + 操作审计 + 破坏性操作二次确认"保证，
// 不再依赖路径白名单。
const (
	// maxFsListEntries 单次列目录的**扫描**上限：排序需要全量，先留一道安全阀。
	// 真正回传给服务端的是其中一页（见 defaultFsPageSize/maxFsPageSize）。
	maxFsListEntries = 5000
	// 分页：单页最多 1000（页大小由服务端的系统配置决定，默认 100）。
	maxFsPageSize = 1000
	maxFsPathLen  = 4096
	// maxCopyDepth 递归复制的最大深度：防止符号链接成环时无限递归。
	maxCopyDepth = 32
)

func (c *Client) handleFsOp(req protocol.FsOpReq) {
	resp := runFsOp(req, c.cfg.AgentID)
	if !resp.OK {
		logx.Warnf("[agent] 文件操作失败 op=%s path=%q dest=%q: %s", req.Op, req.Path, req.Dest, resp.Message)
	}
	_ = c.write(protocol.TypeFsOpResp, resp)
}

// runFsOp 是文件操作的纯逻辑（便于单测覆盖）。
func runFsOp(req protocol.FsOpReq, agentID string) protocol.FsOpResp {
	resp := protocol.FsOpResp{RequestID: req.RequestID, Op: strings.ToLower(strings.TrimSpace(req.Op)), AgentID: agentID, Path: req.Path}
	fail := func(msg string) protocol.FsOpResp {
		resp.OK = false
		resp.Message = msg
		return resp
	}
	ok := func(msg string) protocol.FsOpResp {
		resp.OK = true
		resp.Message = msg
		return resp
	}

	switch resp.Op {
	case "roots":
		resp.Entries = fsRoots()
		resp.Path = "/"
		return ok("")

	case "list":
		// 空路径 / Windows 上的 "/" 表示"列出可浏览的根"；
		// Unix 上 "/" 就是根目录本身，正常列其内容。
		if strings.TrimSpace(req.Path) == "" || (runtime.GOOS == "windows" && strings.TrimSpace(req.Path) == "/") {
			resp.Entries = fsRoots()
			resp.Total = len(resp.Entries)
			resp.Path = "/"
			resp.Platform = runtime.GOOS
			if req.WithDisk {
				resp.Volumes = fsVolumes()
			}
			return ok("")
		}
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		st, err := os.Stat(p) // 跟随符号链接：列一个指向目录的链接是合理操作
		if err != nil {
			return fail("目录不存在或无权限: " + err.Error())
		}
		if !st.IsDir() {
			return fail("不是目录")
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return fail("读取目录失败: " + err.Error())
		}
		sort.SliceStable(entries, func(i, j int) bool {
			di, dj := entries[i].IsDir(), entries[j].IsDir()
			if di != dj {
				return di
			}
			return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
		})
		out := make([]protocol.FileEntry, 0, len(entries))
		for _, e := range entries {
			if len(out) >= maxFsListEntries {
				resp.Truncated = true
				break
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			fe := protocol.FileEntry{
				Name:    e.Name(),
				Path:    filepath.Join(p, e.Name()),
				Dir:     e.IsDir(),
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
				// DirEntry.Info 对符号链接返回链接本身的信息（不跟随目标）。
				Symlink: info.Mode()&os.ModeSymlink != 0,
			}
			// 符号链接到目录（/bin、/lib …）要能被"进入"：DirEntry.IsDir 对链接恒为 false，
			// 这里跟随一次目标判断，并清掉链接自身的字节数（否则界面显示 "7 B"）。
			if fe.Symlink {
				if st, serr := os.Stat(fe.Path); serr == nil && st.IsDir() {
					fe.TargetDir = true
					fe.Size = 0
				}
			}
			// Unix 上顺带带上权限与属主，右击菜单的「权限/所有者」要用；
			// 列目录时一次解析好，避免再发一轮请求。
			if mode, uid, gid, ok := fsEntryMeta(info); ok {
				fe.Mode, fe.UID, fe.GID = mode, uid, gid
				fe.Owner, fe.Group = fsUserName(uid), fsGroupName(gid)
			}
			out = append(out, fe)
		}
		// 分页：排序后切一页；total 用于前端算总页数（扫描上限内）。
		total := len(out)
		offset := req.Offset
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		// Limit<=0 表示"不分页"：旧版服务端不会带这个字段，保持整份返回，
		// 不能悄悄只给 100 条。新版服务端总会带页大小（≤1000）。
		limit := req.Limit
		if limit <= 0 {
			limit = total
		}
		if limit > maxFsPageSize {
			limit = maxFsPageSize
		}
		end := offset + limit
		if end > total {
			end = total
		}
		resp.Entries = out[offset:end]
		resp.Total = total
		resp.Path = p
		resp.Platform = runtime.GOOS
		if req.WithDisk {
			resp.Volumes = fsVolumes()
		}
		if parent := filepath.Dir(p); parent != p {
			resp.Parent = parent
		}
		return ok("")

	case "disk":
		resp.Volumes = fsVolumes()
		if len(resp.Volumes) == 0 {
			return fail("读取磁盘信息失败")
		}
		return ok("")

	case "readtext":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		content, used, detected, size, modTime, err := readTextFile(p, req.Encoding, textLimit(req.MaxBytes))
		if err != nil {
			return fail(err.Error())
		}
		resp.Content, resp.Encoding, resp.Detected = content, used, detected
		resp.Size, resp.ModTime = size, modTime
		return ok("")

	case "writetext":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		size, used, err := writeTextFile(p, req.Content, req.Encoding, textLimit(req.MaxBytes))
		if err != nil {
			return fail(err.Error())
		}
		resp.Size, resp.Encoding = size, used
		return ok("已保存")

	case "create":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		if req.IsDir {
			if err := os.Mkdir(p, 0o755); err != nil {
				return fail("新建目录失败: " + err.Error())
			}
			return ok("目录已创建")
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fail("新建文件失败: " + err.Error())
		}
		_ = f.Close()
		return ok("文件已创建")

	case "rename":
		src, dst, err := resolveRenameTargets(req.Path, req.Dest)
		if err != nil {
			return fail(err.Error())
		}
		if err := os.Rename(src, dst); err != nil {
			return fail("重命名失败: " + err.Error())
		}
		return ok("已重命名为 " + filepath.Base(dst))

	case "copy":
		src, dst, err := resolveCopyTargets(req.Path, req.Dest)
		if err != nil {
			return fail(err.Error())
		}
		if err := copyPath(src, dst, 0); err != nil {
			return fail("复制失败: " + err.Error())
		}
		return ok("已复制到 " + dst)

	case "move":
		src, dst, err := resolveCopyTargets(req.Path, req.Dest)
		if err != nil {
			return fail(err.Error())
		}
		if err := movePath(src, dst); err != nil {
			return fail("移动失败: " + err.Error())
		}
		return ok("已移动到 " + dst)

	case "chmod":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		if isFsRoot(p) {
			return fail("拒绝修改文件系统根的权限")
		}
		if err := applyFsChmod(p, req.Mode); err != nil {
			return fail(err.Error())
		}
		return ok("权限已修改为 " + req.Mode)

	case "chown":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		if isFsRoot(p) {
			return fail("拒绝修改文件系统根的所有者")
		}
		if err := applyFsChown(p, req.Owner, req.Group); err != nil {
			return fail(err.Error())
		}
		return ok("所有者/分组已修改")

	case "delete":
		p, err := fsAbsPath(req.Path)
		if err != nil {
			return fail(err.Error())
		}
		// 拒绝删除文件系统根：误点一次就会把整台机器毁掉，而"删根"没有正当运维场景。
		if isFsRoot(p) {
			return fail("拒绝删除文件系统根目录")
		}
		if err := os.RemoveAll(p); err != nil {
			return fail("删除失败: " + err.Error())
		}
		return ok("已删除 " + p)

	default:
		return fail("不支持的文件操作: " + req.Op)
	}
}

// fsAbsPath 校验并清理路径：必须是非空、无 NUL、不超长的绝对路径。
func fsAbsPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("路径为空")
	}
	if len(raw) > maxFsPathLen || strings.ContainsRune(raw, 0) {
		return "", errors.New("路径不合法")
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("必须是绝对路径")
	}
	return filepath.Clean(raw), nil
}

// isFsRoot 判断是否是文件系统根（Unix "/" 或 Windows 盘符根 "C:\"）。
func isFsRoot(p string) bool {
	clean := filepath.Clean(p)
	if filepath.Dir(clean) == clean {
		return true
	}
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(clean)
		if vol != "" && strings.EqualFold(strings.TrimSuffix(clean, `\`), strings.TrimSuffix(vol, `\`)) {
			return true
		}
	}
	return false
}

// fsRoots 返回可浏览的根：Windows 列存在的盘符，Unix 返回 "/"。
func fsRoots() []protocol.FileEntry {
	if runtime.GOOS != "windows" {
		st, _ := os.Lstat("/")
		e := protocol.FileEntry{Name: "/", Path: "/", Dir: true}
		if st != nil {
			e.ModTime = st.ModTime().Unix()
		}
		return []protocol.FileEntry{e}
	}
	var out []protocol.FileEntry
	for c := 'A'; c <= 'Z'; c++ {
		p := string(c) + `:\`
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		out = append(out, protocol.FileEntry{
			Name: string(c) + ":", Path: p, Dir: true, ModTime: st.ModTime().Unix(),
		})
	}
	return out
}

// resolveRenameTargets 校验重命名的源与目标：只允许同一目录内改名，
// 且目标不能已存在 —— 覆盖别人的文件是不可恢复的误操作，文件管理器不该替用户做这个决定。
func resolveRenameTargets(rawSrc, rawDst string) (string, string, error) {
	src, err := fsAbsPath(rawSrc)
	if err != nil {
		return "", "", fmt.Errorf("源路径：%w", err)
	}
	dst, err := fsAbsPath(rawDst)
	if err != nil {
		return "", "", fmt.Errorf("新路径：%w", err)
	}
	if !sameFsPath(filepath.Dir(src), filepath.Dir(dst)) {
		return "", "", errors.New("重命名只能改同一目录下的名字，跨目录请用移动")
	}
	base := filepath.Base(dst)
	if base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		return "", "", errors.New("新名字不合法")
	}
	if isFsRoot(src) {
		return "", "", errors.New("拒绝重命名文件系统根目录")
	}
	if _, err := os.Lstat(src); err != nil {
		return "", "", fmt.Errorf("源不存在: %v", err)
	}
	if sameFsPath(src, dst) {
		if src == dst {
			return "", "", errors.New("新名字与原名字相同")
		}
		// Windows 上只改大小写（a.txt → A.txt）是有效重命名：
		// 路径比较不区分大小写，会把它误判成"目标已存在"。
	} else if _, err := os.Lstat(dst); err == nil {
		return "", "", errors.New("目标已存在: " + base)
	}
	return src, dst, nil
}

// resolveCopyTargets 解析 copy/move 的源与最终目标：
// 目标若已存在且是目录，则放进去并沿用源的文件名。
func resolveCopyTargets(rawSrc, rawDst string) (string, string, error) {
	src, err := fsAbsPath(rawSrc)
	if err != nil {
		return "", "", fmt.Errorf("源路径：%w", err)
	}
	dst, err := fsAbsPath(rawDst)
	if err != nil {
		return "", "", fmt.Errorf("目标路径：%w", err)
	}
	if _, serr := os.Stat(src); serr != nil {
		return "", "", fmt.Errorf("源不存在: %v", serr)
	}
	if fi, derr := os.Stat(dst); derr == nil && fi.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	if sameFsPath(src, dst) {
		return "", "", errors.New("源和目标相同")
	}
	// 目标不能落在源目录内部（否则递归复制会无限增长）
	if fi, derr := os.Stat(src); derr == nil && fi.IsDir() && fsPathInside(dst, src) {
		return "", "", errors.New("不能把目录复制/移动到自身内部")
	}
	return src, dst, nil
}

func sameFsPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// fsPathInside 判断 p 是否位于 dir 之下。
func fsPathInside(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	prefix := dir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
	}
	return strings.HasPrefix(p, prefix)
}

// copyPath 递归复制文件/目录（符号链接按目标内容复制）。
func copyPath(src, dst string, depth int) error {
	if depth > maxCopyDepth {
		return errors.New("目录层级过深（可能存在符号链接环）")
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := os.Mkdir(dst, st.Mode().Perm()); err != nil && !os.IsExist(err) {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// movePath 优先 rename（同盘瞬时完成）；跨盘时退化为"复制 + 删源"。
func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyPath(src, dst, 0); err != nil {
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("已复制到目标，但删除源失败: %v", err)
	}
	return nil
}
