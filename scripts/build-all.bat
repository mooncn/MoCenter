@echo off
REM =============================================================
REM MoCenter 一键构建脚本 (Windows)
REM 构建 web / server / agent 三大部分
REM 用法: scripts\build-all.bat
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

echo.
echo [1/3] Building Server ...
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

echo.
echo [2/3] Building Agent (Windows amd64) ...
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

echo.
echo [3/3] Building Web ...
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

echo.
echo ==========================================
echo  Build finished. Artifacts in: %OUT_DIR%
dir /b "%OUT_DIR%"
echo ==========================================
endlocal
