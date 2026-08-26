@echo off
REM =============================================================
REM MoCenter Agent Windows 多架构打包脚本
REM 一键交叉编译 x86(386) / x86_64(amd64) / ARM(arm) / ARM64(arm64)
REM 产物输出到 build\agent\windows\ 目录
REM
REM 用法（在装有 Go 的机器上执行）: build-win-all.bat
REM =============================================================
@setlocal enabledelayedexpansion

set "ROOT_DIR=%~dp0.."
set "AGENT_DIR=%ROOT_DIR%\agent"
set "OUT_DIR=%ROOT_DIR%\build\agent\windows"

if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

echo ==========================================
echo  MoCenter Agent Windows 多架构构建
echo  输出目录: %OUT_DIR%
echo ==========================================

where go >nul 2>nul
if %errorlevel% neq 0 (
  echo !! 未找到 Go，请先安装 Go 1.22+
  exit /b 1
)

pushd "%AGENT_DIR%"
go mod download

REM ---- x86_64 (amd64) ----
echo [1/4] 构建 x86_64 (amd64) ...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-agent-amd64.exe" .
echo   -^> %OUT_DIR%\mocenter-agent-amd64.exe

REM ---- x86 (386) ----
echo [2/4] 构建 x86 (386) ...
set GOARCH=386
go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-agent-386.exe" .
echo   -^> %OUT_DIR%\mocenter-agent-386.exe

REM ---- ARM64 ----
echo [3/4] 构建 ARM64 (arm64) ...
set GOARCH=arm64
go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-agent-arm64.exe" .
echo   -^> %OUT_DIR%\mocenter-agent-arm64.exe

REM ---- ARM ----
echo [4/4] 构建 ARM (arm) ...
set GOARCH=arm
set GOARM=7
go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-agent-arm.exe" .
echo   -^> %OUT_DIR%\mocenter-agent-arm.exe

REM 复制安装脚本
copy /y "%ROOT_DIR%\scripts\packaging\install-windows.bat" "%OUT_DIR%\install-windows.bat" >nul
echo   -^> %OUT_DIR%\install-windows.bat

popd

echo.
echo ==========================================
echo  构建完成，产物位于: %OUT_DIR%
dir /b "%OUT_DIR%"
echo ==========================================
echo  部署: 将对应架构的 .exe 命名为 mocenter-agent.exe，
echo        然后在目标 Windows 主机以管理员运行 install-windows.bat
endlocal
