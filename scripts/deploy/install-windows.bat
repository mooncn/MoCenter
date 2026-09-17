@echo off
REM =============================================================
REM MoCenter Windows 服务注册脚本（需管理员）
REM
REM 用法:
REM   install-windows.bat install <server> [agentid] [proxy]
REM   install-windows.bat install-server [user] [password] [listen] [http]
REM   install-windows.bat status | uninstall
REM
REM 示例:
REM   install-windows.bat install your.server.com:9000
REM   install-windows.bat install your.server.com:9000 host-01
REM   install-windows.bat install-server admin MyPass :9000 :8080
REM
REM 说明:
REM   · 优先使用图形安装程序或 mocenter-deploy.exe（走 SCM 握手，服务状态可被
REM     sc query / 服务管理器正确观察）；本脚本是直接用 sc create 的备选路径。
REM   · 二进制默认取自本脚本同目录（免安装包解压后即为该布局）：
REM       <同目录>\bin\mocenter-<role>.exe  或  <同目录>\mocenter-<role>.exe
REM =============================================================
@setlocal enabledelayedexpansion

set "ACTION=%~1"
set "SERVER=%~2"
set "AGENT_ID=%~3"
set "PROXY_IP=%~4"
set "INSTALL_DIR=%ProgramFiles%\MoCenter"
set "HERE=%~dp0"
if "%HERE:~-1%"=="\" set "HERE=%HERE:~0,-1%"

if "%ACTION%"=="" goto usage
if /i "%ACTION%"=="status" goto status
if /i "%ACTION%"=="uninstall" goto uninstall
if /i "%ACTION%"=="install" goto install_agent
if /i "%ACTION%"=="install-server" goto install_server
if /i "%ACTION%"=="install-agent" goto install_agent

echo 未知操作: %ACTION%
goto usage

REM ---------------------------------------------------------------- Agent
:install_agent
if "%SERVER%"=="" (
  echo 请指定服务端地址，如: install-windows.bat install your.server.com:9000
  exit /b 1
)
call :require_admin || exit /b 1
call :find_bin agent || exit /b 1

echo [1/4] 复制 Agent 二进制到 %INSTALL_DIR% ...
call :ensure_dir "%INSTALL_DIR%"
copy /y "%BIN_SRC%" "%INSTALL_DIR%\mocenter-agent.exe" >nul

echo [2/4] 生成配置文件 ...
(
  echo # MoCenter Agent 配置
  echo MOCENTER_SERVER=%SERVER%
  if not "%AGENT_ID%"=="" echo MOCENTER_AGENT_ID=%AGENT_ID%
  if not "%PROXY_IP%"=="" echo MOCENTER_PROXY_IP=%PROXY_IP%
) > "%INSTALL_DIR%\mocenter-agent.conf"

echo [3/4] 注册服务 MoCenterAgent ...
sc stop MoCenterAgent >nul 2>&1
sc delete MoCenterAgent >nul 2>&1
REM 程序通过 svc.IsWindowsService() 自动识别被 SCM 拉起，不需要 NSSM，也不需要 -daemon
sc create MoCenterAgent binPath= "\"%INSTALL_DIR%\mocenter-agent.exe\" -server %SERVER% -agentid %AGENT_ID% -proxy-ip %PROXY_IP% -config \"%INSTALL_DIR%\mocenter-agent.conf\"" start= auto DisplayName= "MoCenter Agent" >nul
if errorlevel 1 ( echo   服务创建失败，请确认以管理员运行 & exit /b 1 )

echo [4/4] 启动服务 ...
sc failure MoCenterAgent reset= 86400 actions= restart/5000/restart/5000/restart/5000 >nul 2>&1
sc start MoCenterAgent >nul 2>&1
echo.
echo OK - MoCenter Agent 已注册为系统服务（开机自启）
echo   服务名: MoCenterAgent
echo   二进制: %INSTALL_DIR%\mocenter-agent.exe
echo   配置:   %INSTALL_DIR%\mocenter-agent.conf
echo   服务端: %SERVER%
goto end

REM ---------------------------------------------------------------- Server
:install_server
call :require_admin || exit /b 1
set "S_USER=%~2"
set "S_PASS=%~3"
set "S_LISTEN=%~4"
set "S_HTTP=%~5"
if "%S_USER%"=="" set "S_USER=admin"
if "%S_PASS%"=="" set "S_PASS=change-me"
if "%S_LISTEN%"=="" set "S_LISTEN=:9000"
if "%S_HTTP%"=="" set "S_HTTP=:8080"
call :find_bin server || exit /b 1

echo [1/3] 复制 Server 二进制到 %INSTALL_DIR% ...
call :ensure_dir "%INSTALL_DIR%"
copy /y "%BIN_SRC%" "%INSTALL_DIR%\mocenter-server.exe" >nul

echo [2/3] 注册服务 MoCenterServer ...
sc stop MoCenterServer >nul 2>&1
sc delete MoCenterServer >nul 2>&1
sc create MoCenterServer binPath= "\"%INSTALL_DIR%\mocenter-server.exe\" -listen %S_LISTEN% -http %S_HTTP% -user %S_USER% -password %S_PASS% -data \"%ProgramData%\MoCenter\server\"" start= auto DisplayName= "MoCenter Server" >nul
if errorlevel 1 ( echo   服务创建失败，请确认以管理员运行 & exit /b 1 )

echo [3/3] 启动服务 ...
sc failure MoCenterServer reset= 86400 actions= restart/5000/restart/5000/restart/5000 >nul 2>&1
sc start MoCenterServer >nul 2>&1
echo.
echo OK - MoCenter Server 已注册为系统服务（开机自启）
echo   后台账号: %S_USER%  ^(请登录后立即修改口令^)
echo   监听: %S_LISTEN% ^(Agent^) / %S_HTTP% ^(Web 控制台^)
goto end

REM ---------------------------------------------------------------- 公共子过程
:require_admin
net session >nul 2>&1
if errorlevel 1 (
  echo 请以管理员身份运行此脚本（右键 - 以管理员身份运行）
  exit /b 1
)
exit /b 0

:ensure_dir
if not exist "%~1" mkdir "%~1"
exit /b 0

REM find_bin <role> —— 依次在本目录、bin\ 子目录中查找二进制
:find_bin
set "BIN_SRC="
for %%r in (%~1) do (
  if exist "%HERE%\mocenter-%%r.exe" set "BIN_SRC=%HERE%\mocenter-%%r.exe"
  if not defined BIN_SRC if exist "%HERE%\bin\mocenter-%%r.exe" set "BIN_SRC=%HERE%\bin\mocenter-%%r.exe"
  if not defined BIN_SRC (
    echo 未找到 mocenter-%%r.exe，请把它放在本脚本同目录或其 bin\ 子目录中
    exit /b 1
  )
)
echo   使用二进制: %BIN_SRC%
exit /b 0

:status
sc query MoCenterAgent 2>nul | findstr /i "SERVICE_NAME STATE" || echo 服务 MoCenterAgent 未安装
sc query MoCenterServer 2>nul | findstr /i "SERVICE_NAME STATE" || echo 服务 MoCenterServer 未安装
goto end

:uninstall
sc stop MoCenterAgent >nul 2>&1
sc delete MoCenterAgent >nul 2>&1
sc stop MoCenterServer >nul 2>&1
sc delete MoCenterServer >nul 2>&1
echo 已删除服务 MoCenterAgent / MoCenterServer
echo 程序目录 %INSTALL_DIR% 与数据目录 %ProgramData%\MoCenter 已保留，如需彻底清理请手动删除
goto end

:usage
echo 用法:
echo   install-windows.bat install ^<server^> [agentid] [proxy]
echo   install-windows.bat install-server [user] [password] [listen] [http]
echo   install-windows.bat status ^| uninstall
goto end

:end
endlocal
