# scripts 目录说明

构建与打包**只有两个入口**，参数同名同义，其余文件都是它们的实现细节或运行时工具。

| 文件 | 用途 |
|------|------|
| `build-linux-all.sh` | 统一构建/打包入口（Linux/macOS 构建机）。Server/Agent/Web + `.deb`/`.rpm`/`zip` |
| `build-all.bat` | 统一构建入口（Windows 构建机）。Server/Agent/Web + 免安装 zip + 图形安装程序 |
| `build-windows-installer.bat` | 被 `build-all.bat --type=installer` 调用；也可单独运行（需 Inno Setup 6） |
| `lib/common.sh` | 公共库：参数校验、版本读取、架构映射、打包工具探测、zip 打包/校验 |
| `deploy/` | **部署期**资产：systemd 单元与配置模板、Linux/Windows 服务注册脚本 |
| `gen-cert.sh` | 生成 CA / 服务端 / 客户端证书（需 openssl） |
| `gencerts/` | 证书生成的 Go 实现（**Windows 无需 openssl**） |
| `fetch-packages.sh` | 把 VNC 安装包放进服务端托管目录 |
| `mcmd/` | 远程命令调试工具（REST + WebSocket） |

## 为什么这样分

- **构建类脚本合并**：此前 `scripts/` 与 `scripts/packaging/` 下有 11 个构建/打包脚本，
  每个都自带一套 `ROOT_DIR`、架构表、版本号与输出目录，加一种架构或改一处版本要改 5 个文件。
  现在统一成两个入口 + 一个公共库。
- **部署类脚本保留**：`deploy/` 下的脚本是在**目标机**上运行的（注册服务、开关机自启），
  与构建机上的打包流程无关，合进构建脚本只会让"该在哪儿执行"变模糊。
- **模板集中**：systemd 单元与配置模板放在 `deploy/`，安装包与安装脚本**引用同一份**，
  避免"打包脚本改了、安装脚本忘改"。

## 常用命令

```bash
bash scripts/build-linux-all.sh --help      # 全部参数
bash scripts/build-linux-all.sh --list      # 全部可选值
bash scripts/build-linux-all.sh             # 默认：全部类型/平台/架构/安装包
```

```bat
scripts\build-all.bat --help
scripts\build-all.bat
```

产物统一落在 `build/`（可用 `--outdir=` 改），先按 `linux/` `windows/` 分类，再按
`binaries/` `web/` `packages/` `installer/` 分类。
