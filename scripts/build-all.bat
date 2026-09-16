@echo off
REM =============================================================
REM MoCenter one-shot build script (Windows)
REM Builds: server / agent / web / Windows GUI setup
REM Usage: scripts\build-all.bat
REM
REM NOTE: this file is deliberately ASCII-only. cmd.exe reads .bat files
REM using the system ANSI codepage, so non-ASCII text gets mangled and can
REM break line parsing. User-facing Chinese text lives in the Inno script.
REM =============================================================
setlocal enabledelayedexpansion

set ROOT_DIR=%~dp0..
set OUT_DIR=%ROOT_DIR%\build

echo ==========================================
echo  MoCenter build start
echo  Root: %ROOT_DIR%
echo  Output: %OUT_DIR%
echo ==========================================

if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

echo(
echo [1/4] Building Server ...
where go >nul 2>nul
if %errorlevel%==0 (
  set "GOOS="
  set "GOARCH="
  set "CGO_ENABLED=0"
  call :build_module "%ROOT_DIR%\server" "%OUT_DIR%\mocenter-server.exe"
) else (
  echo   !! Go not found, skip Server build
)

echo(
echo [2/4] Building Agent (Windows amd64) ...
where go >nul 2>nul
if %errorlevel%==0 (
  set "GOOS=windows"
  set "GOARCH=amd64"
  set "CGO_ENABLED=0"
  call :build_module "%ROOT_DIR%\agent" "%OUT_DIR%\mocenter-agent.exe"
  set "GOOS="
  set "GOARCH="
) else (
  echo   !! Go not found, skip Agent build
)

echo(
echo [3/4] Building Web ...
where npm >nul 2>nul
if %errorlevel%==0 (
  pushd "%ROOT_DIR%\web"
  call npm install --no-audit --no-fund
  call npm run build
  popd
  if exist "%OUT_DIR%\web" rmdir /s /q "%OUT_DIR%\web"
  xcopy /e /i /q "%ROOT_DIR%\web\dist" "%OUT_DIR%\web" >nul
  echo   -^> %OUT_DIR%\web
) else (
  echo   !! Node.js/npm not found, skip Web build
)

echo(
echo [4/4] Building Windows GUI Setup (Inno Setup) ...
REM All setup-related work (build engine, collect payload, invoke ISCC) lives in
REM build-setup.bat so it is defined in exactly one place. It requires Inno
REM Setup 6 and prints guidance instead of failing hard when ISCC is missing.
REM Deliberately a single-line call: multi-line "if ( ... )" blocks are the most
REM fragile construct in cmd, so keep this file as boring as possible.
if exist "%ROOT_DIR%\installer\setup.iss" call "%ROOT_DIR%\scripts\packaging\build-setup.bat"
if not exist "%ROOT_DIR%\installer\setup.iss" echo   ** installer\setup.iss not found, skip

echo(
echo ==========================================
echo  Build finished. Artifacts in: %OUT_DIR%
dir /b "%OUT_DIR%"
echo ==========================================
endlocal

goto :eof

REM =============================================================
REM :build_module <module dir> <output file>
REM
REM Go build with a self-healing module cache. A truncated or corrupted entry in
REM %GOMODCACHE% makes go print "zip: not a valid zip file" and then fail EVERY
REM import resolved from that module (golang.org/x/sys, winapp_windows.go, ...):
REM it looks like a broken source tree but is purely a bad local download.
REM
REM Recovery: clear the cache and retry once through the mirror proxy
REM (goproxy.cn mirrors proxy.golang.org completely).
REM
REM GOFLAGS=-mod=mod is deliberately NOT set anywhere: go.sum must stay the
REM source of truth for third-party code in a remote-control product.
REM =============================================================
:build_module
set "MOD_DIR=%~1"
set "MOD_OUT=%~2"
echo   [go] downloading modules in %MOD_DIR% ...
pushd "%MOD_DIR%"
go mod download
if errorlevel 1 (
  echo   !! go mod download failed - clearing module cache and retrying via mirror proxy
  go clean -modcache
  set "GOPROXY=https://goproxy.cn,direct"
  go mod download
  if errorlevel 1 (
    popd
    echo   ** module download still failing. Check the network, or set GOPROXY
    echo      manually ^(set GOPROXY=https://goproxy.cn,direct^) and re-run.
    exit /b 1
  )
)
echo   [go] building %MOD_OUT% ...
go build -ldflags "-s -w" -o "%MOD_OUT%" .
set "BUILD_RC=%errorlevel%"
if not "%BUILD_RC%"=="0" (
  echo   !! build failed - clearing module cache and retrying via mirror proxy
  go clean -modcache
  set "GOPROXY=https://goproxy.cn,direct"
  go build -ldflags "-s -w" -o "%MOD_OUT%" .
  set "BUILD_RC=%errorlevel%"
)
popd
if not "%BUILD_RC%"=="0" (
  echo   ** build failed for %MOD_DIR%
  exit /b 1
)
echo   -^> %MOD_OUT%
exit /b 0
