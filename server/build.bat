@echo off
REM MoCenter Server 构建脚本 (Windows)
cd /d "%~dp0"
echo [server] go mod download ...
go mod download
echo [server] building mocenter-server.exe ...
go build -ldflags "-s -w" -o mocenter-server.exe .
echo [server] done -^> .\mocenter-server.exe
