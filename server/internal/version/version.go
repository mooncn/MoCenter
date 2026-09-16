// Package version 集中管理服务端版本号。
//
// 为什么要有这个包（而不是散落的字符串常量）：
//
//   - 服务端与 Agent 的版本号必须**统一**：前端「关于」页、/api/version、
//     Agent 上线时上报的版本、升级包校验都要用同一个数。散着写必然出现
//     "服务端 1.0.1 而 Agent 还认为自己是 1.0.0"，升级判定的行为随调用点而异。
//   - 版本号是**发布纪律**，不是运行时的一个配置项：只在明确发版时修改，
//     因此这里没有 -version 命令行参数、没有环境变量覆盖 —— 免得线上跑着
//     一个"版本号被临时改过"的二进制，导致升级判定错乱。
//
// 升级方式：改 Version 常量 + 同步 agent/internal/version 下的同名常量。
// 两处一致性由 server/internal/version 的单元测试守护（读取 agent 源码比对）。
package version

// Version 当前 MoCenter 版本号（服务端与 Agent 共用同一版本号）。
//
// 发布纪律：**只有在明确发版时才修改此值**，且必须与
// agent/internal/version.Version 保持一致。
const Version = "1.0.1"

// Name 产品名（用于「关于」页与 User-Agent 等展示）。
const Name = "MoCenter"

// Full 返回完整的版本标识，如 "MoCenter/1.0.1"。
func Full() string { return Name + "/" + Version }
