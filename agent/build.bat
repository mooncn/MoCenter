@echo off
REM MoCenter Agent 构建脚本 (Windows)
REM 用法: build.bat [386|amd64|arm|arm64]
REM       默认构建 amd64
cd /d "%~dp0"

set "ARCH=%~1"
if "%ARCH%"=="" set "ARCH=amd64"
set GOOS=windows
set GOARCH=%ARCH%
set CGO_ENABLED=0

echo [agent] building mocenter-agent.exe (GOARCH=%GOARCH%) ...
go mod download
if "%ARCH%"=="arm" set GOARM=7
go build -ldflags "-s -w" -o mocenter-agent.exe .
echo [agent] done -^> .\mocenter-agent.exe
