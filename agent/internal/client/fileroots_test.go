package client

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mocenter/agent/internal/protocol"
)

// 本文件覆盖 M-9 的"服务端不能任意读写本机路径"这一半：
// 文件分发/取回的每条路径都必须落在本机配置的根目录内，
// 而扩大根目录的能力只在命令行/配置文件上，通道自己说了不算。

func TestWithinRootsAcceptsInside(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub", "pkg.bin")
	got, err := withinRoots([]string{root}, inside)
	if err != nil {
		t.Fatalf("根目录内路径应放行: %v", err)
	}
	// 返回值必须是绝对路径且仍以根目录结尾（根目录本身可能是软链接，
	// 比较基准用 resolveRoot 解开后的形态，而不是传入的原串）
	realRoot, rerr := resolveRoot(root)
	if rerr != nil {
		t.Fatalf("解析根目录失败: %v", rerr)
	}
	if !filepath.IsAbs(got) || !strings.HasPrefix(got, realRoot+string(filepath.Separator)) {
		t.Fatalf("归一结果异常: %q（真实根 %q）", got, realRoot)
	}
	// 根目录本身也算在内（覆盖"下发到根目录"的合法用法）
	if _, err := withinRoots([]string{root}, root); err != nil {
		t.Fatalf("根目录自身应放行: %v", err)
	}
	// 多余的分隔符与 . 段不该把合法路径判成越界
	if _, err := withinRoots([]string{root}, filepath.Join(root, ".", "a", "..", "b.txt")); err != nil {
		t.Fatalf("规范化后的根内路径应放行: %v", err)
	}
}

func TestWithinRootsRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	cases := map[string]string{
		"根外绝对路径":    filepath.Join(outside, "shadow"),
		"点段逃逸":      filepath.Join(root, "..", "..", "etc", "passwd"),
		"同名前缀的兄弟目录": root + "-escape",
		"相对路径":      "etc/passwd",
		"空路径":       "",
		"只有空白":      "   ",
		"含 NUL":     filepath.Join(root, "a\x00b"),
	}
	for name, p := range cases {
		if _, err := withinRoots([]string{root}, p); err == nil {
			t.Errorf("%s: 应被拒绝，路径 %q", name, p)
		}
	}
	// 根目录没配 = 一个都不放行（fail-closed），而不是"没限制"
	if _, err := withinRoots(nil, filepath.Join(root, "x")); err == nil {
		t.Fatal("没有任何根时必须拒绝（fail-closed）")
	}
	// 根目录还不存在（默认沙箱首次使用前就是这种状态）：仍然把它当被钉住的前缀，
	// 根内放行、根外照旧拒绝 —— 既不炸掉首次下发，也不因为写错一个字就放开全盘
	notYet := filepath.Join(root, "sandbox")
	if _, err := withinRoots([]string{notYet}, filepath.Join(notYet, "a.txt")); err != nil {
		t.Fatalf("尚未创建的根目录应作为前缀放行，实际 %v", err)
	}
	if _, err := withinRoots([]string{notYet}, filepath.Join(root, "a.txt")); err == nil {
		t.Fatal("根目录之外的路径仍必须拒绝")
	}
	// 根目录存在但是个普通文件：配置写错了，不能猜运维想写哪儿
	asFile := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(asFile, []byte("x"), 0600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	if _, err := withinRoots([]string{asFile}, filepath.Join(asFile, "a.txt")); err == nil {
		t.Fatal("根是个普通文件时必须拒绝")
	}
}

// TestWithinRootsOnlyUsesConfiguredRoots 第二个根目录里的路径同样放行 ——
// 多个根是运维常见需求（Linux 上 /var/lib/mocenter 与 /opt/apps 都要能写）。
func TestWithinRootsOnlyUsesConfiguredRoots(t *testing.T) {
	r1, r2, other := t.TempDir(), t.TempDir(), t.TempDir()
	roots := []string{r1, "", r2} // 空项要能容忍，配置文件里手写逗号很常见
	if _, err := withinRoots(roots, filepath.Join(r2, "a.txt")); err != nil {
		t.Fatalf("第二个根内的路径应放行: %v", err)
	}
	if _, err := withinRoots(roots, filepath.Join(other, "a.txt")); err == nil {
		t.Fatal("未列出的目录必须拒绝")
	}
}

// TestWithinRootsRejectsSymlinkEscape 根目录里放一个指向外部的软链接，
// 顺着它写文件不能算"在根内"。
func TestWithinRootsRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "authorized_keys"), []byte("x"), 0600); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前平台/权限不允许创建符号链接: %v", err)
	}
	if _, err := withinRoots([]string{root}, filepath.Join(link, "authorized_keys")); err == nil {
		t.Fatal("经符号链接逃出根目录的路径必须拒绝（字符串层面它是干净的）")
	}
	// 根目录本身是软链接时，判定基准应是解开后的真实目录
	realRoot := outside
	if _, err := withinRoots([]string{realRoot}, filepath.Join(outside, "authorized_keys")); err != nil {
		t.Fatalf("真实根内路径被误拒: %v", err)
	}
}

func TestDefaultFileRoot(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mocenter-agent.json")
	got := DefaultFileRoot(conf)
	if filepath.Base(got) != "files" || filepath.Dir(got) != filepath.Clean(dir) {
		t.Fatalf("默认沙箱应是 <配置目录>/files，实际 %q", got)
	}
	// 配置路径为空时不能退化成相对路径 —— 那会让闸门随工作目录漂移
	if r := DefaultFileRoot(""); !filepath.IsAbs(r) {
		t.Fatalf("默认根必须是绝对路径，实际 %q", r)
	}
}

// TestGuardPathUsesClientConfig 生效根来自本机配置，与请求内容无关。
func TestGuardPathUsesClientConfig(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	c := &Client{cfg: Config{FileRoots: []string{root}}}
	if _, err := c.guardPath(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("配置根内应放行: %v", err)
	}
	_, err := c.guardPath(filepath.Join(other, "a.txt"))
	if err == nil {
		t.Fatal("配置外的目录必须拒绝")
	}
	// 拒绝理由要带上"当前允许哪些根"：运维看到日志就知道去哪儿改，而不是猜
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("错误信息应回显当前根目录，实际: %v", err)
	}
}

// TestFileRootsDefaultWhenUnset 不配 -file-roots 时也要有一个沙箱根，
// 而不是"没有根 = 全放行"。
func TestFileRootsDefaultWhenUnset(t *testing.T) {
	dir := t.TempDir()
	c := New(Config{AgentID: "a", ConfigPath: filepath.Join(dir, "mocenter-agent.json")})
	roots := c.FileRoots()
	if len(roots) != 1 || filepath.Base(roots[0]) != "files" {
		t.Fatalf("默认根应为 <配置目录>/files，实际 %v", roots)
	}
	if _, err := c.guardPath(filepath.Join(roots[0], "x")); err != nil {
		t.Fatalf("默认根内应放行: %v", err)
	}
	if _, err := c.guardPath(filepath.Join(dir, "elsewhere")); err == nil {
		t.Fatal("默认沙箱之外的路径应被拒绝")
	}
}

// newPushClient 造一个只带"回执输出到 buf"的 Client，用于走 handleFilePush 全路径。
func newPushClient(t *testing.T, roots []string, buf *bytes.Buffer) *Client {
	t.Helper()
	return &Client{
		cfg:       Config{AgentID: "test-agent", FileRoots: roots},
		enc:       json.NewEncoder(buf),
		pushFiles: make(map[string]*os.File),
	}
}

func decodePushAck(t *testing.T, buf *bytes.Buffer) protocol.FilePushAck {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("回执信封解析失败: %v (%s)", err, buf.String())
	}
	if env.Type != protocol.TypeFilePushAck {
		t.Fatalf("回执类型 %q", env.Type)
	}
	var ack protocol.FilePushAck
	if err := json.Unmarshal(env.Data, &ack); err != nil {
		t.Fatalf("回执负载解析失败: %v", err)
	}
	return ack
}

// TestHandleFilePushRejectsOutsideRoot 越界路径既不能落盘，也必须把原因回给操作者。
//
// 静默丢弃的表现是"传输条一直卡在进行中"，是最难排查的一类失败；
// 这里要求明确回 rejected + 原因。
func TestHandleFilePushRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "authorized_keys")

	var buf bytes.Buffer
	c := newPushClient(t, []string{root}, &buf)
	c.handleFilePush(protocol.FilePushReq{
		CommandID: "cmd-1", FilePath: target, FileSize: 10, ChunkSeq: 0, ChunkData: []byte("0123456789"),
	})

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("越界路径被写入了（err=%v）", err)
	}
	ack := decodePushAck(t, &buf)
	if ack.Ack || ack.Status != "rejected" || !strings.Contains(ack.Message, "文件根目录") {
		t.Fatalf("应回 rejected 并说明原因，实际 %+v", ack)
	}
}

// TestHandleFilePushWritesInsideRoot 根内的正常路径不受影响：分片落盘、Done 收尾。
func TestHandleFilePushWritesInsideRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "pkg.bin")

	var buf bytes.Buffer
	c := newPushClient(t, []string{root}, &buf)
	c.handleFilePush(protocol.FilePushReq{
		CommandID: "cmd-2", FilePath: target, FileSize: 5, ChunkSeq: 0, ChunkData: []byte("abcde"), Done: true,
	})
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("根内文件未写入: %v", err)
	}
	if string(got) != "abcde" {
		t.Fatalf("内容不符: %q", got)
	}
	if ack := decodePushAck(t, &buf); !ack.Ack || ack.Status != "done" {
		t.Fatalf("应回 done，实际 %+v", ack)
	}
}

// TestSendFileChunksRefusesOutsideRoot 取回方向同样受限 —— 否则这道闸门
// 只挡住了"往哪儿写"，没挡住"读走 /etc/shadow"。
func TestSendFileChunksRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(secret, []byte("root:$1$xx"), 0600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	var buf bytes.Buffer
	c := newPushClient(t, []string{root}, &buf)
	c.sendFileChunks(protocol.FileDownloadReq{CommandID: "cmd-3", FilePath: secret})

	// 唯一允许外发的是"失败上报"（FileSize=-1），且不得带上任何内容
	var env protocol.Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("应有一条失败上报: %v (%q)", err, buf.String())
	}
	var up protocol.FileUploadReq
	if err := json.Unmarshal(env.Data, &up); err != nil {
		t.Fatalf("上报解析失败: %v", err)
	}
	if up.FileSize != -1 || len(up.ChunkData) != 0 {
		t.Fatalf("越界取回必须整体失败且不外发内容，实际 %+v", up)
	}
}

// TestServerPackageSandboxInsideRoots 服务端托管包（VNC/驱动）必须落在 FileRoots 内，
// 否则文件分发的路径闸门会逐片拒绝（历史 bug 是写 os.TempDir()）。
func TestServerPackageSandboxInsideRoots(t *testing.T) {
	root := t.TempDir()
	c := &Client{cfg: Config{FileRoots: []string{root}}}
	dir := c.sandboxSubdir(".packages")
	if filepath.Dir(dir) != root {
		t.Fatalf("专用目录应直接位于 FileRoots 下: %s", dir)
	}
	if _, err := withinRoots(c.cfg.FileRoots, filepath.Join(dir, "UltraVNC_1824_x64_Setup.exe")); err != nil {
		t.Fatalf("托管包落点必须在允许根目录内: %v", err)
	}

	// 未显式配置 FileRoots 时，默认沙箱 <配置目录>/files 同样要成立。
	c2 := New(Config{AgentID: "a", ConfigPath: filepath.Join(root, "mocenter-agent.json")})
	dir2 := c2.sandboxSubdir(".packages")
	if _, err := withinRoots(c2.cfg.FileRoots, filepath.Join(dir2, "pkg.exe")); err != nil {
		t.Fatalf("默认沙箱内的托管包落点应放行: %v", err)
	}
}
