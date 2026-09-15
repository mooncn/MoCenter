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
  pushd "%ROOT_DIR%\server"
  go mod download
  go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-server.exe" .
  popd
  echo   -^> %OUT_DIR%\mocenter-server.exe
) else (
  echo   !! Go not found, skip Server build
)

echo(
echo [2/4] Building Agent (Windows amd64) ...
where go >nul 2>nul
if %errorlevel%==0 (
  pushd "%ROOT_DIR%\agent"
  set GOOS=windows
  set GOARCH=amd64
  set CGO_ENABLED=0
  go mod download
  go build -ldflags "-s -w" -o "%OUT_DIR%\mocenter-agent.exe" .
  popd
  echo   -^> %OUT_DIR%\mocenter-agent.exe
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
