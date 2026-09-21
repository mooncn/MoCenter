// Package command VNC 安装/启动辅助：代理 URL、下载校验等通用小工具。
// 此文件无 build tag，适用于所有平台。
package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

// hasMagic 判断文件是否以指定字节序列开头。
func hasMagic(path string, magic []byte) bool {
	if path == "" || len(magic) == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(magic))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return bytes.Equal(buf, magic)
}

// isPEExecutable 判断是否为 Windows PE 可执行文件（以 "MZ" 开头）。
func isPEExecutable(path string) bool { return hasMagic(path, []byte("MZ")) }

// isELFExecutable 判断是否为 Linux ELF 可执行文件（以 \x7fELF 开头）。
func isELFExecutable(path string) bool {
	return hasMagic(path, []byte{0x7f, 'E', 'L', 'F'})
}

// minVNCBps 下载速度阈值：平均速度低于该值视为慢速，切换备选加速源。
const minVNCBps = 300 * 1024 // 300KB/s

// httpsProxyOrEmpty 过滤加速代理前缀：只放行 https://，其余（含空）返回空串并留日志。
//
// 只接受 https:// 的原因：这条链路的产物最终会被静默执行（安装为服务 / 加载驱动），
// 明文代理等于让任意中间人替换安装包，而"加速代理"通常是第三方运维自建的域名。
// 不合规的配置直接忽略 —— 宁可退回直连，也不走明文。
func httpsProxyOrEmpty(proxy, reason string) string {
	p := strings.TrimSpace(proxy)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(p), "https://") {
		log.Printf("[vnc-install] 忽略非 https 的加速代理（%s）：明文代理可被中间人替换安装包，等同于放弃完整性校验", reason)
		return ""
	}
	return p
}

// 安装包的规范文件名。三处用途必须共用同一常量，否则会出现「取到了包却对不上摘要」：
//   - 向服务端托管目录索取的包名；
//   - 公网下载 URL 的文件名（各平台实现里由前缀 + 本常量派生）；
//   - installPins 的键。
//
// 放在无 build tag 的文件里，是为了让钉扎表与这些名字在同一编译单元内互相约束
// （见 TestInstallPinsCoverAllPackages），任何平台跑 go test 都能发现漂移。
const (
	// uvncSetupX64Name / uvncSetupX86Name UltraVNC 官方 Inno Setup 安装包
	//（文件名取自 winget-pkgs 清单 uvncbvba.UltraVNC 1.8.2.4）。
	uvncSetupX64Name = "UltraVNC_1824_x64_Setup.exe"
	uvncSetupX86Name = "UltraVNC_1824_x86_Setup.exe"
	// vddZipName VirtualDrivers/Virtual-Display-Driver 的 x86 驱动包（GitHub Release 资产名）。
	vddZipName = "VirtualDisplayDriver-x86.Driver.Only.zip"
)

// installPins 内置安装包摘要钉扎表：包名 → SHA-256（小写十六进制）。
//
// 为什么必须钉扎：这些包的落点是以 SYSTEM 静默执行的安装器，以及 pnputil 加载的
// 内核模式驱动。取包链路（公网直连 / 运维配的加速代理 / 镜像）不由我们选源与选版本，
// 只验 `MZ` 魔数等于「任何能改写这条响应的人都能在全网 Agent 上执行代码」。
//
// 摘要来源（均为公开可复核）：
//   - UltraVNC：winget-pkgs 官方清单 manifests/u/uvncbvba/UltraVNC/1.8.2.4 的
//     InstallerSha256，其 InstallerUrl 与本仓库 uvncSetup*URL 指向同一文件；
//   - VDD：从 GitHub Release 25.7.23 直下后计算（zip 完整性已校验）。
//
// 上游换版本会体现为**文件名里的版本号变化**，所以正常升级不会撞旧钉扎；真撞上时
// 错误消息会打印实际摘要，运维确认来源可信后改本表、随 Agent 正常升级下发。
var installPins = map[string]string{
	uvncSetupX64Name: "9ef5c8275aa0431f058369305c444d7303adc807a9e4b80bc9ffad7c3bf210b2",
	uvncSetupX86Name: "3354dca22896104c0018bb74fff7028fb5c8e2a497a49e811b529444cf487bfa",
	vddZipName:       "e24210692b442b39af763536330ce78b423f19342b7a7792c26de3944e418b3a",
}

// pinnedDigest 返回包名对应的内置摘要；表里没有则返回空串（表示不校验）。
func pinnedDigest(name string) string { return installPins[name] }

// publicDownloadHint 公网取包未解锁时的统一补充说明（各平台安装链共用）。
//
// 默认关闭公网取包不是嫌麻烦：它把「装哪个版本的什么内容」交给第三方源。
// 服务端托管走已 mutual-TLS 的长连接，且摘要照样校验，两条性质都没有。
// 开关只在本机配置里读，服务端下发不了 —— 否则等于让服务端一键放弃这条防护。
func publicDownloadHint(cfg VncConfig) string {
	if cfg.AllowPublicDownload {
		return ""
	}
	return "；公网取包（winget/choco/厂商直连）默认关闭，确需时在本机 Agent 配置里设 " +
		"allow_public_package_download=true（该开关只能本机设置）"
}

// proxyURL 将原始下载地址套上国内加速代理前缀（如 https://mirror.example.com/?url= ）。
func proxyURL(proxy, raw string) string {
	if strings.Contains(proxy, "?url=") {
		return proxy + urlEncodeStr(raw)
	}
	base := strings.TrimRight(proxy, "/")
	return base + "/?url=" + urlEncodeStr(raw)
}

// sha256File 计算文件摘要（小写十六进制）。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyDigest 按钉扎摘要校验已落盘的包：want 为空表示该包没有内置钉扎（不校验）。
//
// 摘要不符时的错误消息会**带上实际摘要**：这样运维确认来源可信后，
// 只需把摘要更新进钉扎表即可，不用再去手工算一遍。
func verifyDigest(path, want, label string) error {
	if want == "" {
		return nil
	}
	got, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("计算 %s 摘要失败: %w", label, err)
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(want)), []byte(got)) != 1 {
		return fmt.Errorf("%s 与内置钉扎摘要不符（期望 %s，实际 %s），已拒绝执行："+
			"下载链路被替换、或上游已换版本；若确认来源可信，请把实际摘要更新进 installPins",
			label, digestPrefix(want), digestPrefix(got))
	}
	return nil
}

// digestPrefix 摘要取前缀用于展示（过短或异常值不能截崩错误消息）。
func digestPrefix(h string) string {
	h = strings.TrimSpace(h)
	if len(h) <= 16 {
		return h
	}
	return h[:16] + "…"
}

// psQuote 把字符串包成 PowerShell 单引号字面量。
// 这些 URL 有一部分来自配置（-proxy-ip / 镜像地址），不转义单引号就等于把
// 引号闭合权交给配置值 —— 在 SYSTEM 服务上下文里那是直接的命令注入。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// urlEncodeStr 对 URL 做最小化编码（仅编码路径与查询保留符）。
func urlEncodeStr(s string) string {
	r := strings.NewReplacer(" ", "%20", "?", "%3F", "&", "%26", "=", "%3D", "#", "%23", "%", "%25")
	return r.Replace(s)
}

// downloadSlowAware 在后台执行 start(ctx) 下载到 out；若平均/增量速率持续低于
// minBps，则取消本次下载并返回错误，由调用方切换到备选加速源。
// start 需在 ctx 取消时中止底层下载进程。
func downloadSlowAware(out string, minBps float64, start func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- start(ctx) }()

	startT := time.Now()
	last := int64(0)
	for {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			if fi, e := os.Stat(out); e == nil && fi.Size() > 0 {
				return nil
			}
			return fmt.Errorf("%s 下载不完整", out)
		case <-time.After(time.Second):
			elapsed := time.Since(startT).Seconds()
			size := fileSizeOf(out)
			if elapsed > 2 { // 给 2s 提速期
				bps := float64(size) / elapsed
				delta := float64(size - last)
				if bps < minBps && delta < minBps {
					cancel()
					return fmt.Errorf("下载速度过慢(%.0f B/s <%.0f B/s)，切换下载源", bps, minBps)
				}
			}
			last = size
			if elapsed > 90 {
				cancel()
				return fmt.Errorf("下载超时(>90s)")
			}
		}
	}
}

func fileSizeOf(p string) int64 {
	if fi, e := os.Stat(p); e == nil {
		return fi.Size()
	}
	return 0
}
