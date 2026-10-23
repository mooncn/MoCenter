# 构建双平台安装包并发布到 GitHub Releases

结论先说：**Windows 的图形安装程序只能在 Windows 上编译**，Linux 的 `.deb/.rpm` 需要在 Linux 上打包；
所以“一次把两套都编出来”最省事的做法是 **GitHub Actions 双 job 构建**（本仓库已带
`.github/workflows/release.yml`）。没有 CI 时，也可以在 Windows 与 Linux 各跑一次脚本，再把产物合并上传。

## 0. 产物清单

| 平台 | 位置 | 产物 |
|---|---|---|
| Windows | `build/bin/server/windows/` | `mocenter-server-<arch>.exe` |
| Windows | `build/bin/agent/windows/` | `mocenter-agent-<arch>.exe` |
| Windows | `build/packages/zip/` | `mocenter-{server,agent}-<版本>-windows-<arch>.zip`（免安装包） |
| Windows | `build/setup/` | `MoCenter-Setup-<版本>.exe`（图形安装程序） |
| Linux | `build/bin/{server,agent}/linux/` | `mocenter-{server,agent}-<arch>` |
| Linux | `build/packages/deb/` | `mocenter-{server,agent}_<版本>_<debarch>_<arch>.deb` |
| Linux | `build/packages/rpm/` | `mocenter-{server,agent}-<版本>-1.<rpmarch>.rpm` |
| 共用 | `build/web/` | 前端静态资源（两平台同一份） |

> `build/` 已在 `.gitignore`，不要提交；发布时只上传安装包/压缩包，不要把整个 `build/` 推上去。

## 1. 有 CI：打一个 tag 就够（推荐）

前提：仓库已推到 GitHub（当前 `origin` 是 cnb.cool，需要额外加一个 GitHub remote）：

```bash
git remote add github https://github.com/<你的账号>/MoCenter.git
git push github master
```

发布新版本：

```bash
git tag v1.0.4
git push github v1.0.4        # 触发 .github/workflows/release.yml
```

流程：

1. **windows job**（windows-latest）：装 Go/Node/Inno Setup → `scripts\build-all.bat --clean --version=1.0.4`
   → 产出 Windows 二进制、zip、Setup.exe；
2. **linux job**（ubuntu-latest）：装 rpm/fakeroot → `bash scripts/build-linux-all.sh --clean --platform=linux --package=all --version=1.0.4`
   → 产出 Linux 二进制、deb、rpm；
3. **publish job**：下载两份 artifact，创建 Release 并附上 `*.zip`、`*.exe`、`*.deb`、`*.rpm`，
   自动生成 release notes。

只想验证构建、不发版：Actions 页面选 **Release → Run workflow**（`workflow_dispatch`，不会创建 Release）。

## 2. 没有 CI：两台机器各编一半

### Windows 机器

```bat
cd /d D:\Documents\MoCenter
scripts\build-all.bat --clean --version=1.0.4
```

- 需要 Go（≥ go.mod 要求）、Node ≥ 18、Inno Setup 6（`ISCC.exe`，脚本会自动找常见安装路径）。
- 产物：`build\bin\**\*.exe`、`build\packages\zip\*.zip`、`build\setup\MoCenter-Setup-1.0.4.exe`。

### Linux 机器

```bash
cd /path/to/MoCenter
sudo apt-get install -y rpm fakeroot        # Debian/Ubuntu；RHEL 系用 dnf install rpm-build
bash scripts/build-linux-all.sh --clean --platform=linux --package=all --version=1.0.4
```

- 产物：`build/packages/deb/*.deb`、`build/packages/rpm/*.rpm`、`build/bin/*/linux/*`。
- 该脚本 `--platform=all` 时也能顺带交叉编译 **Windows 二进制与 zip**，
  但 **Setup.exe 仍需要 Windows + Inno Setup**（脚本会提示跳过）。

### 合并与上传

把两台的 `build/packages`、`build/setup` 汇总到同一台机器，然后任选一种：

```bash
# 用 gh CLI（需先 gh auth login）
gh release create v1.0.4 \
  build/packages/zip/*.zip \
  build/setup/MoCenter-Setup-1.0.4.exe \
  build/packages/deb/*.deb \
  build/packages/rpm/*.rpm \
  --title "MoCenter v1.0.4" --generate-notes
```

或直接在 GitHub 网页 **Releases → Draft a new release → 选择 tag → 上传附件**。

## 3. 常见问题

| 现象 | 处理 |
|---|---|
| Windows job 报找不到 ISCC | 确认 `choco install innosetup` 成功；或给 runner 设 `ISCC_PATH` |
| Linux 打包报缺 `rpmbuild` | 装 `rpm`（Ubuntu）或 `rpm-build`（RHEL） |
| Release 里出现同名附件冲突 | 同一种 zip 不要两边都生成：CI 里 Linux 只传 deb/rpm，Windows 只传 zip/exe |
| 版本号不对 | tag 去掉 `v` 传给 `--version`；Server/Agent 版本各自独立，可用 `--agent-version` 单独指定 |
| 想只发部分平台 | 手动跑脚本时用 `--type=server` / `--platform=linux` 等参数裁剪 |

## 4. 与产品内升级的关系

GitHub Release 的资产可以给用户手工下载安装；**在线升级**走 MoCenter 自己的通道：

- 本地 pkg 升级：把升级包放进服务端 `data/packages`（免签名，视为可信）；
- 在线链接升级：Agent 必须配置 `-upgrade-pubkey`，升级包需用对应私钥签名。

详见使用手册 [07 运维操作](../wiki/07-运维操作.md)。
