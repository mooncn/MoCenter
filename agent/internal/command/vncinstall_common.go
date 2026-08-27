// Package command VNC 安装/启动辅助：代理 URL、慢速下载检测等通用小工具。
// 此文件无 build tag，适用于所有平台。
package command

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// minVNCBps 下载速度阈值：平均速度低于该值视为慢速，切换备选加速源。
const minVNCBps = 300 * 1024 // 300KB/s

// proxyURL 将原始下载地址套上国内加速代理前缀（如 http://fa.mooncn.win/?url= ）。
func proxyURL(proxy, raw string) string {
	if strings.Contains(proxy, "?url=") {
		return proxy + urlEncodeStr(raw)
	}
	base := strings.TrimRight(proxy, "/")
	return base + "/?url=" + urlEncodeStr(raw)
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