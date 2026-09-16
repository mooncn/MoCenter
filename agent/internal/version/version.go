// Package version 集中管理 Agent 版本号。
//
// 版本号必须与 server/internal/version.Version **保持一致**：
// 服务端据此判断"该 Agent 是否需要升级"，前端也用它展示在线主机的版本。
// 一致性由 server/internal/version 包中的单元测试守护。
package version

// Version 当前 Agent 版本号（与服务端共用同一版本号）。
//
// 发布纪律：**只有在明确发版时才修改此值**。
const Version = "1.0.1"

// Name 产品名。
const Name = "MoCenter"

// Full 返回完整的版本标识，如 "MoCenter-Agent/1.0.1"。
func Full() string { return Name + "-Agent/" + Version }
