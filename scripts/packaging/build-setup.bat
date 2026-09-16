@echo off
REM =============================================================
REM MoCenter Windows GUI Setup builder (Inno Setup 6)
REM
REM Usage: scripts\packaging\build-setup.bat
REM
REM Steps:
REM   1) Build agent / server / deploy engine into installer\payload\
REM   2) Copy the built web frontend (web\dist) into installer\payload\web\
REM   3) Compile installer\setup.iss with ISCC -> build\MoCenter-Setup-<ver>.exe
REM
REM This file is deliberately ASCII-only and uses only the plainest cmd syntax.
REM cmd.exe reads .bat files with the system ANSI codepage, so non-ASCII text
REM gets mangled and can break line parsing. Anything fancy (for /f, delayed
REM expansion, "&" chains) is avoided on purpose: those are the constructs that
REM behave differently across codepages and Windows versions.
REM User-facing Chinese text lives in installer\setup.iss (UTF-8 with BOM).
REM
REM Requires: Inno Setup 6 (https://jrsoftware.org/isdl.php)
REM   Auto-detected on the PATH, under the standard Program Files folders, or in
REM   "Inno Setup 6" on drives C..J. If it sits in a custom folder, set
REM   INNO_SETUP_ISCC to the full path of ISCC.exe before running this script.
REM =============================================================
setlocal

set "ROOT_DIR=%~dp0..\.."
set "PAYLOAD=%ROOT_DIR%\installer\payload"
set "OUT_DIR=%ROOT_DIR%\build"

echo ==========================================
echo  MoCenter Windows GUI Setup builder
echo ==========================================

REM ---- Locate ISCC ----
REM Inno Setup does not have to live on the system drive, so the two "standard"
REM paths under %ProgramFiles% are only the first guess. Search order:
REM   1) INNO_SETUP_ISCC environment variable (explicit override)
REM   2) PATH                      -- via "for %%p in (ISCC.exe) do %%~$PATH:p"
REM   3) %ProgramFiles(x86)% and %ProgramFiles%
REM   4) "Program Files [x86]\Inno Setup 6" on each drive C..J
REM Only a non-standard folder on some other drive needs the override.
set "ISCC="
if defined INNO_SETUP_ISCC if exist "%INNO_SETUP_ISCC%" set "ISCC=%INNO_SETUP_ISCC%"
if not defined ISCC for %%p in (ISCC.exe) do set "ISCC=%%~$PATH:p"
if not defined ISCC if exist "%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe" set "ISCC=%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe"
if not defined ISCC if exist "%ProgramFiles%\Inno Setup 6\ISCC.exe" set "ISCC=%ProgramFiles%\Inno Setup 6\ISCC.exe"
for %%d in (C D E F G H I J) do if not defined ISCC if exist "%%d:\Program Files (x86)\Inno Setup 6\ISCC.exe" set "ISCC=%%d:\Program Files (x86)\Inno Setup 6\ISCC.exe"
for %%d in (C D E F G H I J) do if not defined ISCC if exist "%%d:\Program Files\Inno Setup 6\ISCC.exe" set "ISCC=%%d:\Program Files\Inno Setup 6\ISCC.exe"

if not defined ISCC (
  echo(
  echo ** ISCC.exe not found. Please install Inno Setup 6 first:
  echo    https://jrsoftware.org/isdl.php
  echo(
  echo    Already searched: the PATH, the standard Program Files folders, and
  echo    "Inno Setup 6" on drives C..J.
  echo    If it lives somewhere else, set the full path and re-run, e.g.:
  echo      set INNO_SETUP_ISCC=D:\Tools\Inno Setup 6\ISCC.exe
  echo(
  echo    Nothing was built. You can still install from the command line with
  echo    mocenter-deploy.exe ^(see README^).
  exit /b 1
)
echo Using compiler: %ISCC%

where go >nul 2>nul
if errorlevel 1 (
  echo ** Go not found, cannot build the deploy engine.
  exit /b 1
)

if not exist "%PAYLOAD%" mkdir "%PAYLOAD%"
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

REM ---- 1) Build the three binaries into payload ----
echo(
echo [1/3] Building agent / server / deploy engine ...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
REM Agent version: independent from the server version (see internal/version).
REM Override with: set MOCENTER_AGENT_VERSION=1.2.0
if not defined MOCENTER_AGENT_VERSION set "MOCENTER_AGENT_VERSION=1.0.1"
set "AGENT_LDFLAGS=-s -w -X mocenter/agent/internal/version.Version=%MOCENTER_AGENT_VERSION%"

pushd "%ROOT_DIR%\agent"
go build -ldflags "%AGENT_LDFLAGS%" -o "%PAYLOAD%\mocenter-agent.exe" .
if errorlevel 1 (
  popd
  echo ** agent build failed.
  exit /b 1
)
popd

pushd "%ROOT_DIR%\server"
go build -ldflags "-s -w" -o "%PAYLOAD%\mocenter-server.exe" .
if errorlevel 1 (
  popd
  echo ** server build failed.
  exit /b 1
)
popd

pushd "%ROOT_DIR%\installer"
go build -ldflags "-s -w" -o "%PAYLOAD%\mocenter-deploy.exe" .
if errorlevel 1 (
  popd
  echo ** deploy engine build failed.
  exit /b 1
)
popd
echo   done

REM ---- 2) Copy web frontend (if built) ----
echo(
echo [2/3] Copying web frontend ...
if exist "%ROOT_DIR%\web\dist\index.html" (
  if exist "%PAYLOAD%\web" rmdir /s /q "%PAYLOAD%\web"
  xcopy /e /i /q "%ROOT_DIR%\web\dist" "%PAYLOAD%\web" >nul
  echo   done
) else (
  echo ** web\dist not found. The package will not include the Web UI.
  echo    Run scripts\build-all.bat first if you want it bundled.
)

REM ---- 3) Compile the setup wizard ----
echo(
echo [3/3] Compiling setup wizard with ISCC ...
"%ISCC%" "%ROOT_DIR%\installer\setup.iss"
if errorlevel 1 (
  echo(
  echo ** ISCC compilation failed. Please send the error output above.
  exit /b 1
)

echo(
echo ==========================================
echo  Done. Output in %OUT_DIR%:
REM Single-line if instead of an if/else block: this file avoids paren blocks
REM on purpose (see the header note) - an unescaped "(" inside a block breaks
REM cmd's parser.
if exist "%OUT_DIR%\MoCenter-Setup-*.exe" dir /b "%OUT_DIR%\MoCenter-Setup-*.exe"
if not exist "%OUT_DIR%\MoCenter-Setup-*.exe" echo   ** no MoCenter-Setup-*.exe was produced
echo ==========================================
endlocal
REM Reaching here means every earlier step succeeded. Return 0 explicitly so a
REM failing "dir" above cannot be mistaken for a failed build.
exit /b 0
