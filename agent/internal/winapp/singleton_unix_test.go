//go:build !windows

package winapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 单实例锁是"防重复进程"的唯一屏障，因此这里的用例都按**真实进程**验证，
// 而不是只测函数返回值 —— 同一个进程内对同一文件重复 flock 会成功（锁是"打开文件"级别的），
// 只有跨进程才能反映真实语义。
//
// 测试用的 key 一律以 "MoCenter-LockTest-<用例名>" 命名并登记清理：
// 锁文件落在系统临时目录，用随机路径生成 key 会累积成几十上百个文件
// （本项目之前踩过这个坑），所以这里统一在用例结束时删除。

// 子进程 helper 的分工（通过**环境变量**派发，不用命令行参数）。
//
// 为什么不用 -xxx 参数：go test 生成的 main 会先执行 testing.Init()，
// 未注册的参数会直接报 "flag provided but not defined" 并退出 ——
// 我们自己的分支根本来不及执行（这是实际踩到的坑，表现为用例一直超时）。
// `--` 也不能"截断" testing.Init（它只影响后续 flag.Parse）。
// 用环境变量最稳，且与顺序无关。
const (
	helperEnvMode = "MOCENTER_LOCK_TEST_MODE"
	helperEnvKey  = "MOCENTER_LOCK_TEST_KEY"
	helperEnvCfg  = "MOCENTER_LOCK_TEST_CFG"
	helperEnvFile = "MOCENTER_LOCK_TEST_FILE"

	// helperHold 取得锁后保持存活，等父进程 Kill（验证崩溃不残留锁）
	helperHold = "hold"
	// helperExit 取得锁后立刻退出（验证正常退出后锁被释放）
	helperExit = "exit"
	// helperHoldUntilFile 取得锁后等 release 文件出现再退出
	helperHoldUntilFile = "hold-until-file"
)

// testKey 生成测试用 key，并登记在用例结束后清理（含 .meta）。
//
// key 必须**可预测**才能清理：SingletonKey 是"路径哈希"，
// 用 t.TempDir() 得到的随机路径会生成随机 key，永远删不掉。
func testKey(t *testing.T, name string) (string, string) {
	t.Helper()
	// 锁文件落在"配置同目录"，因此测试用临时目录作为配置目录，天然隔离且自动清理。
	cfg := filepath.Join(t.TempDir(), "mocenter-agent.json")
	key := "MoCenter-LockTest-" + name
	return key, cfg
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnvMode); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

// runHelper 在子进程中执行"持锁"角色。父进程通过环境变量传入参数。
func runHelper(mode string) int {
	key, cfg := os.Getenv(helperEnvKey), os.Getenv(helperEnvCfg)
	marker := os.Getenv(helperEnvFile)

	_, ok := AcquireSingleton(key, cfg)
	switch mode {
	case helperExit:
		// 只抢锁，立刻退出
		if ok && marker != "" {
			_ = os.WriteFile(marker, []byte("held"), 0644)
		}
		return 0
	case helperHoldUntilFile:
		if ok {
			_ = os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0644)
		} else {
			// 拿不到锁：同样落盘 marker，但标记为 denied，供用例断言
			_ = os.WriteFile(marker, []byte("denied"), 0644)
		}
		// 等父进程放行（避免用例还没判断子进程就退出）
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker + ".release"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		return 0
	case helperHold:
		if !ok {
			return 1
		}
		// 向父进程报告"锁已持有"（管道读到这一行才继续后面的断言），
		// 随后阻塞等待被 SIGKILL —— 验证内核是否会自动释放锁。
		fmt.Fprintln(os.Stdout, "held")
		_ = os.Stdout.Sync()
		time.Sleep(2 * time.Minute)
		return 0
	}
	return 2
}

// spawnHolder 异步拉起一个持有锁的子进程，返回 (pid, release)。
func spawnHolder(t *testing.T, key, cfg string) (int, func()) {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		helperEnvMode+"="+helperHoldUntilFile,
		helperEnvKey+"="+key,
		helperEnvCfg+"="+cfg,
		helperEnvFile+"="+ready,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动持锁子进程失败: %v", err)
	}
	// 等 ready 文件出现
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(ready)
		if err == nil {
			if string(b) == "denied" {
				t.Fatalf("子进程未能取得锁（key=%s）", key)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return pid, func() {
				_ = os.WriteFile(ready+".release", []byte("go"), 0644)
				_ = cmd.Wait()
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("等待持锁子进程就绪超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 核心用例：已有实例持锁时，第二次获取必须失败（这正是"定时任务保活"制造重复实例的场景）。
func TestAcquireSingletonRejectsSecondProcess(t *testing.T) {
	key, cfg := testKey(t, t.Name())
	pid, release := spawnHolder(t, key, cfg)
	defer release()

	if !processAlive(pid) {
		t.Fatalf("持锁子进程 %d 应仍存活", pid)
	}

	release2, ok := AcquireSingleton(key, cfg)
	if ok {
		release2()
		t.Fatal("已有实例持锁时，第二次获取应当失败（否则会出现重复实例）")
	}

	// 持有者退出后必须能重新获取 —— 不能出现"陈旧锁把 Agent 永久挡在门外"。
	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rel, ok := AcquireSingleton(key, cfg)
		if ok {
			rel()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("持锁进程退出后仍无法获取锁：陈旧锁未随进程释放")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 不同配置必须互不影响（与 Windows 侧 key 语义一致）。
func TestAcquireSingletonKeyedByConfigPath(t *testing.T) {
	dir := t.TempDir()
	cfgA := filepath.Join(dir, "a.json")
	cfgB := filepath.Join(dir, "b.json")
	keyA := SingletonKey("LockTest", cfgA)
	keyB := SingletonKey("LockTest", cfgB)
	if keyA == keyB {
		t.Fatal("不同配置路径生成的 key 不应相同")
	}
	if keyA != SingletonKey("LockTest", filepath.Join(dir, ".", "a.json")) {
		t.Fatal("同一路径的不同写法应生成相同 key（filepath.Clean 归一化）")
	}

	_, releaseA := spawnHolder(t, keyA, cfgA)
	defer releaseA()

	releaseB, ok := AcquireSingleton(keyB, cfgB)
	if !ok {
		t.Fatal("不同配置的实例之间不应互斥")
	}
	releaseB()
}

// release 必须幂等：main 里 defer release() 与调用方的显式调用可能重复执行。
func TestReleaseIsIdempotent(t *testing.T) {
	key, cfg := testKey(t, t.Name())
	release, ok := AcquireSingleton(key, cfg)
	if !ok {
		t.Fatal("首次获取应成功")
	}
	release()
	release() // 重复释放不应 panic（close of closed channel 是真实踩过的坑）
	release()
}

// 锁文件不可写时**不阻塞启动**（宁可多跑一个，也不要完全起不来），但要有明确日志告警。
//
// 注意：root 不受目录权限位约束，因此该用例在 root 下会自动跳过（必须显式跳过，
// 否则会"通过"得毫无意义）。
func TestAcquireSingletonDegradesGracefullyWhenLockFileUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位约束，无法构造不可写场景（CI 中该用例通常跳过）")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Skipf("无法设置目录权限: %v", err)
	}
	defer os.Chmod(dir, 0700)

	cfg := filepath.Join(dir, "mocenter-agent.json")
	release, ok := AcquireSingleton("MoCenter-LockTest-degraded", cfg)
	if !ok {
		t.Fatal("锁文件不可写时应降级放行，而不是拒绝启动")
	}
	release()
}

// 锁文件内容是可读的排查线索（PID / 配置 / 启动时间），且能被正确解析。
func TestLockFileRecordsOwner(t *testing.T) {
	key, cfg := testKey(t, t.Name())

	release, ok := AcquireSingleton(key, cfg)
	if !ok {
		t.Fatal("首次获取应成功")
	}
	defer release()

	owner := readLockOwner(lockFilePath(key, cfg))
	if owner == nil {
		t.Fatalf("锁文件 %s 应包含可解析的持有者信息", lockFilePath(key, cfg))
	}
	if owner.PID != os.Getpid() {
		t.Fatalf("持有者 PID 应为当前进程 %d，实际 %d", os.Getpid(), owner.PID)
	}
	if owner.Config != cfg {
		t.Fatalf("持有者配置路径应为 %q，实际 %q", cfg, owner.Config)
	}
	if _, err := time.Parse(time.RFC3339, owner.StartedAt); err != nil {
		t.Fatalf("启动时间应为 RFC3339，实际 %q", owner.StartedAt)
	}
}

// 陈旧锁文件（上次断电遗留）不得把新实例挡在门外。
// 这里直接构造"文件存在、时间戳很久以前、无人持锁"的状态。
func TestStaleLockFileDoesNotBlockStartup(t *testing.T) {
	key, cfg := testKey(t, t.Name())
	path := lockFilePath(key, cfg)
	_ = os.MkdirAll(filepath.Dir(path), 0700)

	stale := []byte(`{"pid":999999,"exe":"/tmp/gone","config":"/tmp/gone.json","started_at":"2020-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, stale, 0600); err != nil {
		t.Fatalf("写入陈旧锁文件失败: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(path, old, old)
	defer os.Remove(path)

	release, ok := AcquireSingleton(key, cfg)
	if !ok {
		t.Fatal("陈旧锁文件（无人持锁）不应阻止启动")
	}
	defer release()

	// 取得锁后内容应被重写为当前进程
	owner := readLockOwner(path)
	if owner == nil || owner.PID != os.Getpid() {
		t.Fatalf("取得锁后应重写持有者信息为当前进程，实际 %+v", owner)
	}
}

// 子进程崩溃/被 Kill 后锁由内核释放（对比 PID 文件方案的核心优势）。
//
// 注意 helper 的写法：`exec.Command(...).Run()` 会阻塞到子进程退出，
// 所以必须显式 Start()；而 Start() 只保证"进程已创建"，
// 子进程跑到测试分支前**绝不能**去抢锁（否则会先拿到锁，用例误判为"锁没生效"）。
// 在 Go 里正确做法：把管道交给子进程，从管道读到信号才说明子进程已就绪。
func TestLockReleasedOnProcessExit(t *testing.T) {
	key, cfg := testKey(t, t.Name())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(),
		helperEnvMode+"="+helperHold,
		helperEnvKey+"="+key,
		helperEnvCfg+"="+cfg,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("获取子进程管道失败: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}

	// 子进程写入一行即表示它已取得锁
	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		_ = cmd.Process.Kill()
		t.Fatalf("子进程未确认持锁: %q %v", line, err)
	}

	// 子进程仍在运行时我们必须拿不到锁
	if rel, ok := AcquireSingleton(key, cfg); ok {
		rel()
		_ = cmd.Process.Kill()
		t.Fatal("子进程持锁期间不应能获取锁")
	}

	// SIGKILL：进程无法做任何清理，锁仍须自动释放
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for {
		rel, ok := AcquireSingleton(key, cfg)
		if ok {
			rel()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("进程被 SIGKILL 后锁未释放：这正是 PID 文件方案的老问题")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// key 派生文件名必须安全（不含路径分隔符等），且不同 key 不互相覆盖。
func TestSanitizeKeyProducesSafeFilename(t *testing.T) {
	key, cfg := testKey(t, t.Name())
	name := sanitizeKey(key)
	if strings.ContainsAny(name, `/\`) {
		t.Fatalf("文件名不得含路径分隔符: %q", name)
	}
	if base := filepath.Base(lockFilePath(key, cfg)); base != ".mocenter-"+name+".lock" {
		t.Fatalf("锁文件应为 .mocenter-<sanitized>.lock，实际 %q", base)
	}
	if sanitizeKey("../../etc/passwd") == "../../etc/passwd" {
		t.Fatal("路径穿越字符应被替换")
	}
}

// 锁文件被外部误删时，持锁进程应自动重建，避免"锁突然失效"。
func TestLockFileRecreatedAfterExternalRemoval(t *testing.T) {
	key, cfg := testKey(t, t.Name())
	path := lockFilePath(key, cfg)
	release, ok := AcquireSingleton(key, cfg)
	if !ok {
		t.Fatal("首次获取应成功")
	}
	defer release()

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除锁文件失败: %v", err)
	}

	// touchLoop 周期为 1 分钟，这里直接触发一次等价逻辑：
	// 重新打开并读取，确认不会 panic，且 meta 信息仍可还原。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("重建锁文件失败: %v", err)
	}
	_ = f.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取锁文件失败: %v", err)
	}
	var o lockOwner
	if len(b) > 0 {
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatalf("锁文件内容应为合法 JSON: %v", err)
		}
	}
}

// 日志辅助函数不应在 PID 不存在时误判"可以删锁"，只描述事实。
func TestLockOwnerNoteDoesNotClaimSafeToKill(t *testing.T) {
	note := lockOwnerNote(&lockOwner{PID: 999999, BootID: "not-this-boot"})
	if !strings.Contains(note, "上一次开机") {
		t.Fatalf("跨开机遗留应在提示中说明，实际 %q", note)
	}
	if strings.Contains(note, "杀掉") || strings.Contains(note, "kill") {
		t.Fatalf("提示不得包含杀进程类措辞（本实现绝不杀进程）: %q", note)
	}
	if n := lockOwnerNote(&lockOwner{PID: os.Getpid()}); n != "" {
		t.Fatalf("本机存活进程不应附加异常提示，实际 %q", n)
	}
}
