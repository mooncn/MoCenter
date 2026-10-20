package main

import (
	"testing"

	"mocenter/server/internal/portmap"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/share"
)

// 分享访客不能在总控上占用系统保留端口：那会把基础服务（SSH/HTTP…）直接顶掉。
func TestSharePortmapRejectsPrivilegedPublicPort(t *testing.T) {
	a := &shareAPI{pm: portmap.NewManager(nil, nil)}
	sh := &share.Share{ID: "s1", AgentID: "a1"}
	for _, port := range []int{0, 22, 80, 443, 1023, 65536} {
		_, err := a.applyPortmapOp(sh, &protocol.PortMapOp{
			Action: "create", PublicPort: port, LocalPort: 5900,
		})
		if err == nil {
			t.Fatalf("公网端口 %d 应被拒绝", port)
		}
	}
}
