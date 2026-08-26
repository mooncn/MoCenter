@echo off
REM =============================================================
REM MoCenter Agent Windows 安装/注册为系统服务脚本
REM 用法（管理员）:
REM   install-windows.bat install <server> [agentid] [proxy]
REM   install-windows.bat uninstall
REM   install-windows.bat status
REM
REM 示例:
REM   install-windows.bat install your.server.com:9000
REM   install-windows.bat install your.server.com:9000 host-01 http://fa.mooncn.win/?url=
REM =============================================================
@setlocal enabledelayedexpansion

set "ACTION=%~1"
set "SERVER=%~2"
set "AGENT_ID=%~3"
set "PROXY_IP=%~4"
set "SERVICE=MoCenterAgent"
set "INSTALL_DIR=%ProgramFiles%\MoCenter"
set "BIN_NAME=mocenter-agent.exe"
set "BIN_PATH=%INSTALL_DIR%\%BIN_NAME%"
set "CONF_FILE=%INSTALL_DIR%\mocenter-agent.conf"

if "%ACTION%"=="" (
  echo 用法: install-windows.bat [install ^<server^> [agentid] [proxy] ^| uninstall ^| status]
  exit /b 1
)

if "%ACTION%"=="status" goto status
if "%ACTION%"=="uninstall" goto uninstall
if "%ACTION%"=="install" goto install

echo 未知操作: %ACTION%
exit /b 1

:install
if "%SERVER%"=="" (
  echo 请指定服务端地址，如: install-windows.bat install your.server.com:9000
  exit /b 1
)

REM 检查管理员权限
net session >nul 2>&1
if %errorlevel% neq 0 (
  echo 请以管理员身份运行此脚本（右键 - 以管理员身份运行）
  exit /b 1
)

echo [1/4] 复制 Agent 二进制到 %INSTALL_DIR% ...
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"
if exist "%~dp0%mocenter-agent.exe" (
  copy /y "%~dp0%mocenter-agent.exe" "%BIN_PATH%" >nul
) else if exist "%BIN_PATH%" (
  echo   使用已存在的 %BIN_PATH%
) else (
  echo   未找到 mocenter-agent.exe，请先构建（build-all.bat 或 agent\build.bat）
  echo   或将编译好的 mocenter-agent.exe 放在本脚本同目录
  exit /b 1
)

echo [2/4] 生成配置文件 %CONF_FILE% ...
(
  echo # MoCenter Agent 配置
  echo MOCENTER_SERVER=%SERVER%
  if not "%AGENT_ID%"=="" echo MOCENTER_AGENT_ID=%AGENT_ID%
  if not "%PROXY_IP%"=="" echo MOCENTER_PROXY_IP=%PROXY_IP%
) > "%CONF_FILE%"

echo [3/4] 注册系统服务 %SERVICE% ...
REM 先删除可能存在的旧服务
sc stop %SERVICE% >nul 2>&1
sc delete %SERVICE% >nul 2>&1
sc create %SERVICE% binPath= "\"%BIN_PATH%\" -server %SERVER% -agentid %AGENT_ID% -proxy-ip %PROXY_IP% -daemon" start= auto DisplayName= "MoCenter Agent" >nul
if %errorlevel% neq 0 (
  echo   服务创建失败，请确认以管理员运行
  exit /b 1
)

echo [4/4] 设置开机自启并启动服务 ...
sc config %SERVICE% start= auto >nul
sc start %SERVICE% >nul 2>&1
echo.
echo ✅ MoCenter Agent 已安装并注册为系统服务（开机自启）
echo   服务名: %SERVICE%
echo   二进制: %BIN_PATH%
echo   配置:   %CONF_FILE%
echo   服务端: %SERVER%
goto end

:uninstall
echo 停止并删除服务 %SERVICE% ...
sc stop %SERVICE% >nul 2>&1
sc delete %SERVICE% >nul 2>&1
echo 已删除服务（如需删除文件请手动移除 %INSTALL_DIR%）
goto end

:status
sc query %SERVICE% 2>nul | findstr /i "SERVICE_NAME STATE" || echo 服务 %SERVICE% 未安装
sc qc %SERVICE% 2>nul | findstr /i "START_TYPE" || echo 未查询到服务配置
goto end

:end
endlocal
