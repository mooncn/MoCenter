@echo off
REM MoCenter Server build script (Windows)
REM Usage: build.bat [amd64^|arm64^|arm^|386]   (default amd64)
REM Server only runs on Windows on the controlling host, so GOOS is fixed.
cd /d "%~dp0"

set "ARCH=%~1"
if "%ARCH%"=="" set "ARCH=amd64"

set GOOS=windows
set GOARCH=%ARCH%
set CGO_ENABLED=0

echo [server] go mod download ...
go mod download
echo [server] building mocenter-server.exe (GOARCH=%GOARCH%) ...
go build -ldflags "-s -w" -o mocenter-server.exe .
echo [server] done -^> .\mocenter-server.exe
