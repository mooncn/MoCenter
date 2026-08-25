@echo off
REM MoCenter Agent 构建脚本 (Windows)
cd /d "%~dp0"
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
echo [agent] go mod download ...
go mod download
echo [agent] building mocenter-agent.exe ...
go build -ldflags "-s -w" -o mocenter-agent.exe .
echo [agent] done -^> .\mocenter-agent.exe
