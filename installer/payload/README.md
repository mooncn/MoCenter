# payload 目录

本目录是**图形安装程序的打包素材**，供 `installer/setup.iss`（Inno Setup）收集文件用。
构建脚本 `scripts/build-windows-installer.bat`（由 `scripts\build-all.bat --type=installer` 调用）会把下列内容准备齐：

| 文件/目录 | 来源 |
|---|---|
| `mocenter-agent.exe` | `go build` 于 `agent/` |
| `mocenter-server.exe` | `go build` 于 `server/` |
| `mocenter-deploy.exe` | `go build` 于 `installer/`（安装引擎） |
| `web/` | 复制自 `web/dist`（前端，供 Server 托管） |

这些 exe 是构建产物，已被 `.gitignore` 的 `*.exe` 忽略，**不纳入版本控制**。

## 三者分工

- **`MoCenter-Setup-<版本>.exe`（Inno 产物）** —— 用户双击的图形安装向导：
  负责收集参数（角色 / 服务端地址 / 后台登录账号密码 / 端口…）并把文件摆到安装目录。
- **`mocenter-deploy.exe`（本仓库 installer 模块）** —— 安装引擎，负责真正的
  服务注册、配置落盘、单实例保护、日志重定向。向导在文件就位后调用它；
  也可单独用于静默/脚本化批量部署。它**不再内嵌二进制**（那会让体积从 2MB 涨到 21MB），
  文件摆放交给 Inno。

保留本 README 是为了让目录非空（便于说明用途）；`.iss` 里对 `web\` 用了
`skipifsourcedoesntexist`，因此未构建前端时也能正常打包，只是安装包不含 Web 界面。
