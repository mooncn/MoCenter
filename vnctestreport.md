# Windows 无头 VNC 测试报告

## 背景
MoCenter Agent 在 Windows 虚拟机上部署后，VNC 远程桌面显示黑屏。目标是实现**无头（无人连接控制台）Windows 虚拟机的 VNC 远程桌面**。

## 环境
- **Win10 VM**: 192.168.117.131, Administrator/123456, VMware NAT
- **Rocky Linux 8 VM**: 192.168.117.132, root/123456 (作为对照，Linux VNC 已正常工作)
- **服务端**: mocenter-server 运行于宿主机 :9000/:8080
- **Agent**: 位于 `C:\Users\Administrator\Desktop\MoCenter\mocenter-agent.exe`

## 排查过程与踩坑

### 1. TightVNC 服务模式导致 Session 0 黑屏（产品 Bug，已修复）
**现象**：重启后 VNC 全黑，即使有显示器也抓不到画面。

**根因**：TightVNC MSI 安装时会注册一个**自启动服务** (`tvnserver`)，该服务运行在 **Session 0**。Session 0 的 TightVNC 只能抓到空桌面（黑屏）。同时，Agent 安装逻辑中还有一个"服务模式兜底"，会主动拉起这个黑屏服务。

**修复**（`agent/internal/command/vncinstall_windows.go`）：
- 安装后执行 `sc config tvnserver start= disabled`，禁用 TightVNC 服务；
- 删除代码中的"服务模式兜底"分支，只保留交互式会话中的应用模式启动。

**验证**：服务已禁用，tvnserver 由 MoCenter-Logon 任务在交互会话中启动。

---

### 2. Agent 运行在 Session 0，DXGI/GDI 抓取失败
**现象**：通过 Agent 命令拉起的抓屏程序（dxgicapture.exe）无法枚举到任何 DXGI 输出，`EnumOutputs` 返回 `DXGI_ERROR_NOT_CURRENTLY_AVAILABLE`。

**根因**：Agent 进程本身运行在 **Session 0（Services）**，其子进程也继承 Session 0。**DXGI Desktop Duplication 必须在交互会话（Session 1+）中运行**才能访问桌面输出。

**验证方法**：通过计划任务 `/IT` 参数将抓屏程序放入交互会话，成功枚举到输出。

**产品启示**：Agent 的 VNC 抓取组件必须设计为在**交互会话中运行**（如通过 schtasks /it 拉起），不能直接在 Session 0 中抓取。

---

### 3. IddSampleDriver（IDD 驱动）无法被 DXGI Desktop Duplication 捕获
**现象**：在交互会话中安装了 IddSampleDriver 并创建了虚拟显示设备（`ROOT\IDDSAMPLEDRIVER\0000`），`EnumOutputs` 能枚举到虚拟屏 `\\.\DISPLAY9 1280x800 attached=1`，但调用 `IDXGIOutputDuplication::AcquireNextFrame` 时发生**访问越界崩溃（AV, 0xc0000005）**。

**尝试的调试**：
- 确认 D3D11 设备创建成功且指针有效；
- 确认 DuplicateOutput 返回成功；
- 更换帧信息缓冲区（堆分配/栈分配、不同大小）——均无效；
- 跳过离线的主屏（DISPLAY1），只抓虚拟屏（DISPLAY9）——仍然在 AcquireNextFrame 崩溃。

**结论**：**IDD（间接显示驱动）类型的虚拟屏，不支持标准的 DXGI Desktop Duplication 抓取流程**。IDD 的帧是通过驱动内部的"间接"通道交付给接收端的，不暴露为可被 Desktop Duplication 直接读取的桌面表面。这与之前 GDI 轮询也抓不到 IDD 屏的现象一致。

**对比 RustDesk**：RustDesk 的无头方案使用的是**显示器仿真型（display-only/monitor-emulation）驱动**，而非纯 IDD。这类驱动让系统把虚拟屏当作一块真正的物理显示器，DXGI/GDI 都能正常捕获。

---

### 4. 虚拟显示驱动选型
| 驱动 | 类型 | 签名 | 授权 | 评估 |
|---|---|---|---|---|
| **IddSampleDriver** | IDD（间接显示） | 自签 | MIT | ❌ DXGI 抓取崩溃，不适用 |
| **Parsec VDD** | 显示器仿真 | ✅ WHQL | 闭源商业 | ⚠️ 可用但二次分发有授权风险 |
| **VirtualDrivers/Virtual-Display-Driver** | 显示器仿真 | 需测试签名 | 开源 | ✅ 推荐，无授权风险，可集成 |

**推荐方案**：使用 **VirtualDrivers/Virtual-Display-Driver**（开源、可商用），配合 Go DXGI Desktop Duplication 抓取。这是最干净、无法律风险的路径。

---

## 当前状态
- ✅ TightVNC 服务模式 bug 已修复（产品代码已改）
- ✅ 确认了 Agent 抓取必须在交互会话运行的架构约束
- ✅ 验证了 IddSampleDriver 不适用于 DXGI 抓取（技术结论）
- ⏳ 待验证：VirtualDrivers/Virtual-Display-Driver + Go DXGI 抓取链路

## 后续工作
1. 在 Win10 VM 上安装 VirtualDrivers/Virtual-Display-Driver；
2. 用现有 Go DXGI 抓取程序验证能否抓到虚拟屏的画面；
3. 将"装驱动→建设备→DXGI 抓取"固化进 Agent 的 Windows VNC 安装流程；
4. 清理测试脚本和临时文件，提交最终代码。
