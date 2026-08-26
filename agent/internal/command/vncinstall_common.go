// Package command VNC 安装/启动辅助：代理 URL 与通用小工具。
// 此文件无 build tag，适用于所有平台。
package command

import "strings"

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
