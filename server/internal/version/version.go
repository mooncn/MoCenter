// Package version 集中管理服务端版本号。
//
// 为什么要有这个包（而不是散落的字符串常量）：
//
//   - 服务端版本号会出现在备份清单、/api/version、「关于」页等对外口径上，
//     散着写必然出现不同页面显示不同版本号的问题。
//   - 它同时决定安装包文件名与注册表版本（installer/setup.iss 的 AppVersion），
//     集中一处便于发版时同步。
//   - 版本号是**发布纪律**，不是运行时的一个配置项：只在明确发版时修改，
//     因此这里没有 -version 命令行参数、没有环境变量覆盖 —— 免得线上跑着
//     一个"版本号被临时改过"的二进制，导致升级判定错乱。
//
// 与 Agent 版本号的关系：**两者互相独立，不做一致性校验**。
// Agent 版本由 agent/internal/version 单独维护，表达"Agent 功能迭代到哪一代"；
// 服务端只把它当展示字段（AgentInfo.AgentVersion），
// 版本高于或低于服务端都属于正常状态。
package version

// Version 当前 MoCenter 服务端版本号（独立于 Agent 版本号）。
//
// 发布纪律：**只有在明确发版时才修改此值**。
const Version = "1.0.2"

// Name 产品名（用于「关于」页与 User-Agent 等展示）。
const Name = "MoCenter"

// Full 返回完整的版本标识，如 "MoCenter/1.0.2"。
func Full() string { return Name + "/" + Version }
