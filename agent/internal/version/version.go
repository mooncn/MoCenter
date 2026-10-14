// Package version 集中管理 Agent 版本号。
//
// 设计要点：**Agent 版本号与 Server 版本号互相独立**。
//
// 为什么独立（而不是"服务端与 Agent 共用同一版本号"）：
//   - 这个数字表达的是"Agent 自身功能迭代到哪一代"，由 Agent 的发布节奏决定；
//     Server 完全可以只改自己的代码就发版，没必要、也不应该牵动全网 Agent。
//   - 反过来，只改 Agent 时也不需要动 Server 版本，否则会逼着服务端走一次
//     "没有任何实际改动"的发布流程。
//   - 服务端**不**对两者做一致性校验：Agent 版本高于或低于服务端都属于正常状态，
//     它只是一个展示字段 + 升级时的比较对象。
//
// 与升级判定的关系：升级指令里的 version 表示"想升到哪个版本"，
// Agent 只比较它与自身版本是否相等 —— 相等则跳过（除非强制），否则执行升级。
// 因此版本号只需要在**同一台机器的历史序列**里有意义，不需要跨端对齐。
//
// 发布纪律：只有在明确发版时才修改此值；需要临时区分构建产物时用
//   -ldflags "-X mocenter/agent/internal/version.Version=x.y.z"
// 注入（Server 侧不做任何对应改动）。
package version

// Version 当前 Agent 版本号（独立于服务端版本）。
//
// 发布纪律：**只有在明确发版时才修改此值**。
//
// 为什么是 var 而不是 const：const 无法被 `-ldflags -X` 覆盖，
// 于是"用 MOCENTER_AGENT_VERSION 临时打一个不同版本的二进制"就做不到。
// 构建脚本统一按 `${MOCENTER_AGENT_VERSION:-从本文件读到的值}` 注入，
// 所以默认行为与 const 完全一致（不注入就是这里写的值）。
// 运行期不可修改：没有任何命令行参数或环境变量能改它。
var Version = "1.0.2"

// Name 产品名。
const Name = "MoCenter"

// Full 返回完整的版本标识，如 "MoCenter-Agent/1.0.2"。
func Full() string { return Name + "-Agent/" + Version }
