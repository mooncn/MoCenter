@echo off
REM =============================================================
REM MoCenter Windows GUI setup builder (Inno Setup 6)
REM
REM Called by scripts\build-all.bat --type=installer; can also be run
REM directly.  Two positional args, both optional:
REM   %1 = output root      (default <repo>\build)
REM   %2 = agent version    (used for the payload build)
REM
REM Steps:
REM   1) Build agent / server / deploy engine into installer\payload\
REM   2) Copy the built web frontend (web\dist) into installer\payload\web\
REM   3) Compile installer\setup.iss with ISCC -> <out>\setup\
REM
REM This file is deliberately ASCII-only and uses only the plainest cmd syntax.
REM cmd.exe reads .bat files with the system ANSI codepage, so non-ASCII text
REM gets mangled and can break line parsing.  User-facing Chinese text lives in
REM installer\setup.iss (UTF-8 with BOM).
REM
REM Requires: Inno Setup 6 (https://jrsoftware.org/isdl.php)
REM   Auto-detected on the PATH, under the standard Program Files folders, or in
REM   "Inno Setup 6" on drives C..J.  If it sits in a custom folder, set
REM   INNO_SETUP_ISCC to the full path of ISCC.exe before running this script.
REM =============================================================
setlocal

set "ROOT_DIR=%~dp0.."
if "%ROOT_DIR:~-1%"=="\" set "ROOT_DIR=%ROOT_DIR:~0,-1%"
set "PAYLOAD=%ROOT_DIR%\installer\payload"
REM %1 is the build root (build-all.bat passes its --outdir); setup.iss writes
REM into <root>\setup. Do NOT pass ..\build\setup here -- that would add a
REM second "setup".
if "%~1"=="" ( set "OUT_ROOT=%ROOT_DIR%\build" ) else ( set "OUT_ROOT=%~1" )
set "OUT_DIR=%OUT_ROOT%\setup"
set "AGENT_VERSION=%~2"
if "%AGENT_VERSION%"=="" set "AGENT_VERSION=%MOCENTER_AGENT_VERSION%"

echo ==========================================
echo  MoCenter Windows GUI setup builder
echo ==========================================

REM ---- Locate ISCC ----
REM Inno Setup does not have to live on the system drive, so the two "standard"
REM paths under %ProgramFiles% are only the first guess.  Search order:
REM   1) INNO_SETUP_ISCC environment variable (explicit override)
REM   2) PATH                      -- via "for %%p in (ISCC.exe) do %%~$PATH:p"
REM   3) %ProgramFiles(x86)% and %ProgramFiles%
REM   4) "Program Files [x86]\Inno Setup 6" on each drive C..J
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

if "%AGENT_VERSION%"=="" (
  pushd "%ROOT_DIR%\agent"
  go build -ldflags "-s -w" -o "%PAYLOAD%\mocenter-agent.exe" .
) else (
  pushd "%ROOT_DIR%\agent"
  go build -ldflags "-s -w -X mocenter/agent/internal/version.Version=%AGENT_VERSION%" -o "%PAYLOAD%\mocenter-agent.exe" .
)
if errorlevel 1 ( popd & echo ** agent build failed. & exit /b 1 )
popd

pushd "%ROOT_DIR%\server"
go build -ldflags "-s -w" -o "%PAYLOAD%\mocenter-server.exe" .
if errorlevel 1 ( popd & echo ** server build failed. & exit /b 1 )
popd

pushd "%ROOT_DIR%\installer"
go build -ldflags "-s -w" -o "%PAYLOAD%\mocenter-deploy.exe" .
if errorlevel 1 ( popd & echo ** deploy engine build failed. & exit /b 1 )
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
  echo    Add --web=1 to build-all.bat if you want it bundled.
)

REM ---- 3) Compile the setup wizard ----
echo(
echo [3/3] Compiling setup wizard with ISCC ...
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"
"%ISCC%" /O"%OUT_DIR%" "%ROOT_DIR%\installer\setup.iss"
if errorlevel 1 (
  echo(
  echo ** ISCC compilation failed. Please send the error output above.
  exit /b 1
)

echo(
echo ==========================================
echo  Done. Output in %OUT_DIR%:
dir /b "%OUT_DIR%\MoCenter-Setup-*.exe" 2>nul || echo   ** no MoCenter-Setup-*.exe was produced
echo ==========================================
endlocal
exit /b 0
