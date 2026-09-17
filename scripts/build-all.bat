@echo off
REM =============================================================
REM MoCenter unified build script (Windows build host)
REM
REM Option names are identical to scripts/build-linux-all.sh -- see that
REM script (or --help here) for the full table.  This script does NOT
REM cross-build Linux artifacts: .deb needs dpkg-deb, .rpm needs
REM fpm/rpmbuild, and the Linux host is the real target anyway.  Run
REM scripts/build-linux-all.sh there (or in CI) instead.
REM
REM Usage:
REM   scripts\build-all.bat                        all types, all arches
REM   scripts\build-all.bat --type=agent
REM   scripts\build-all.bat --type=server --arch=amd64
REM   scripts\build-all.bat --type=agent --package=zip --arch=amd64,386
REM   scripts\build-all.bat --type=installer
REM   scripts\build-all.bat --web=0 --package=none
REM   scripts\build-all.bat --outdir=D:\mocenter-build
REM   scripts\build-all.bat --list
REM
REM Options:
REM   --type=     agent ^| server ^| web ^| installer ^| none ^| all
REM   --package=  zip ^| none ^| all        (Windows side produces zip)
REM   --platform= windows ^| all           (Linux targets: build-linux-all.sh)
REM   --arch=     amd64 ^| 386 ^| arm64 ^| all   (windows/arm is NOT supported by Go)
REM   --binary=   1 ^| 0
REM   --web=      1 ^| 0 ^| auto
REM   --outdir=   directory                default ^<repo^>\build
REM   --version=  server version           default read from server source
REM   --agent-version= agent version       default read from agent source
REM   --clean     wipe outdir first
REM   --list      print allowed values
REM   --help      show help
REM
REM Artifacts (same layout as the Linux script):
REM   build\windows\binaries\server\windows\mocenter-server-<arch>.exe
REM   build\windows\binaries\agent\windows\mocenter-agent-<arch>.exe
REM   build\windows\web\                       web frontend static files
REM   build\windows\packages\zip\              portable zips (binary+scripts)
REM   build\windows\installer\                 GUI setup (Inno Setup)
REM
REM NOTE: this file is deliberately ASCII-only.  cmd.exe reads .bat files
REM using the system ANSI codepage, so non-ASCII text gets mangled and can
REM break line parsing.
REM
REM TESTING NOTE: this script is written against documented cmd.exe behaviour
REM and covered by the option-parsing checks in CI.  It is NOT reliably
REM testable under Wine: Wine's cmd drops everything after "=" in unquoted
REM arguments (so "--type=agent" arrives as "--type"), which is a Wine defect,
REM not a script bug -- real cmd.exe preserves it.  Verify Windows-side changes
REM on an actual Windows host.
REM
REM STRUCTURE: the main flow ends with "goto :main_done" right before the
REM first subroutine label.  cmd has no block scoping -- a subroutine placed
REM inside the main flow is simply executed as part of it, which is how an
REM earlier revision silently skipped all argument validation.
REM =============================================================
setlocal enabledelayedexpansion

set "ROOT_DIR=%~dp0.."
if "%ROOT_DIR:~-1%"=="\" set "ROOT_DIR=%ROOT_DIR:~0,-1%"

set "OPT_TYPE=all"
set "OPT_PACKAGE=all"
set "OPT_PLATFORM=windows"
set "OPT_ARCH=all"
set "OPT_BINARY=1"
set "OPT_WEB=auto"
set "OPT_OUTDIR="
set "OPT_CLEAN=0"
set "OPT_SERVER_VERSION="
set "OPT_AGENT_VERSION="

REM ---------------------------------------------------------------- parse args
:parse
if "%~1"=="" goto parsed
set "ARG=%~1"
if /i "%ARG%"=="--help"     goto show_help
if /i "%ARG%"=="-h"         goto show_help
if /i "%ARG%"=="--list"     goto show_list
if /i "%ARG%"=="--clean"    ( set "OPT_CLEAN=1" & shift & goto parse )

REM Split "--name=value" into its two halves (first "=" only, since an outdir
REM may legitimately contain "=").
REM
REM The "for /f ... delims==" idiom is avoided on purpose: it is the single
REM most implementation-dependent construct in cmd (token handling differs
REM between Windows versions, and every cmd emulator gets it wrong in a
REM different way).  Instead the "=" is turned into a space and the two halves
REM are read back as positional arguments of a called label -- plain, fully
REM specified behaviour.
set "OPT_KEY="
set "OPT_VALUE="
for %%A in ("%ARG%") do set "ARGPAIR=%%~A"
set "ARGPAIR_SPACED=%ARGPAIR:= %"
if "%ARGPAIR_SPACED%"=="%ARGPAIR%" (
  REM no "=" present: bare "--name value" form
  set "OPT_KEY=%ARGPAIR%"
  for %%A in (%ARGPAIR_SPACED%) do set "OPT_KEY=%%A"
  shift
  if "%~1"=="" goto parse_unknown
  set "OPT_VALUE=%~1"
) else (
  REM The two halves arrive as %1/%2 of :split_pair -- the "=" -> space
  REM rewrite above means the value keeps any further "=" it contains.
  call :split_pair %ARGPAIR_SPACED%
)

set "OPT_FOUND="
if /i "%OPT_KEY%"=="--type"          set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_TYPE=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--package"       set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_PACKAGE=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--platform"      set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_PLATFORM=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--arch"          set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_ARCH=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--binary"        set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_BINARY=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--web"           set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_WEB=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--outdir"        set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_OUTDIR=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--version"       set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_SERVER_VERSION=%OPT_VALUE%"
if /i "%OPT_KEY%"=="--agent-version" set "OPT_FOUND=1" & if defined OPT_FOUND set "OPT_AGENT_VERSION=%OPT_VALUE%"
if not defined OPT_FOUND goto parse_unknown
shift
goto parse

:parse_unknown
echo ** Unknown option: %ARG%  ^(run scripts\build-all.bat --help^)
exit /b 1

:parsed
REM ---------------------------------------------------------------- validate
if "%OPT_PLATFORM%"=="" set "OPT_PLATFORM=windows"
if /i not "%OPT_PLATFORM%"=="windows" if /i not "%OPT_PLATFORM%"=="all" (
  echo ** --platform=%OPT_PLATFORM% is not supported by the Windows script.
  echo    It does not cross-build Linux artifacts: run scripts/build-linux-all.sh
  echo    on a Linux/macOS host ^(or in CI^) instead.
  exit /b 1
)
if /i not "%OPT_PACKAGE%"=="zip" if /i not "%OPT_PACKAGE%"=="none" if /i not "%OPT_PACKAGE%"=="all" (
  echo ** --package=%OPT_PACKAGE% is not supported on Windows: zip ^| none ^| all
  echo    ^(.deb/.rpm are built by scripts/build-linux-all.sh^)
  exit /b 1
)
if /i not "%OPT_BINARY%"=="1" if /i not "%OPT_BINARY%"=="0" (
  echo ** --binary=%OPT_BINARY% is not supported: 1 ^| 0
  exit /b 1
)
if /i not "%OPT_WEB%"=="1" if /i not "%OPT_WEB%"=="0" if /i not "%OPT_WEB%"=="auto" (
  echo ** --web=%OPT_WEB% is not supported: 1 ^| 0 ^| auto
  exit /b 1
)

REM --type -> WANT_* flags
set "WANT_AGENT=0"
set "WANT_SERVER=0"
set "WANT_WEB=0"
set "WANT_INSTALLER=0"
if /i "%OPT_TYPE%"=="all" (
  set "WANT_AGENT=1"
  set "WANT_SERVER=1"
  set "WANT_WEB=1"
  set "WANT_INSTALLER=1"
) else (
  for %%T in (%OPT_TYPE:,= %) do (
    if /i "%%T"=="agent"     set "WANT_AGENT=1"
    if /i "%%T"=="server"    set "WANT_SERVER=1"
    if /i "%%T"=="web"       set "WANT_WEB=1"
    if /i "%%T"=="installer" set "WANT_INSTALLER=1"
    if /i "%%T"=="none"      echo   --type=none: nothing to build
    if /i not "%%T"=="agent" if /i not "%%T"=="server" if /i not "%%T"=="web" if /i not "%%T"=="installer" if /i not "%%T"=="none" (
      echo ** --type=%%T is not supported: agent ^| server ^| web ^| installer ^| none ^| all
      exit /b 1
    )
  )
)
if "%WANT_WEB%"=="1" set "OPT_WEB=1"

REM --arch -> ARCH_LIST
REM NOTE: windows/arm (32-bit ARM) is deliberately NOT included -- it is not a
REM supported Go target (broken port, see go.dev/issue/68552; there is no
REM windows/arm entry in "go tool dist list"), and some toolchains fail outright
REM with: go: unsupported GOOS/GOARCH pair windows/arm
REM Use arm64 for ARM Windows devices.
if /i "%OPT_ARCH%"=="all" set "OPT_ARCH=amd64,386,arm64"
set "ARCH_LIST="
for %%A in (%OPT_ARCH:,= %) do (
  if /i "%%A"=="amd64" set "ARCH_LIST=!ARCH_LIST! amd64"
  if /i "%%A"=="386"   set "ARCH_LIST=!ARCH_LIST! 386"
  if /i "%%A"=="arm64" set "ARCH_LIST=!ARCH_LIST! arm64"
  if /i "%%A"=="arm" (
    echo ** windows/arm is not supported by Go ^(broken port, go.dev/issue/68552^).
    echo    Use --arch=arm64 for ARM Windows devices.
    exit /b 1
  )
  if /i not "%%A"=="amd64" if /i not "%%A"=="386" if /i not "%%A"=="arm64" (
    echo ** --arch=%%A is not supported: amd64 ^| 386 ^| arm64 ^| all
    exit /b 1
  )
)
if defined ARCH_LIST set "ARCH_LIST=%ARCH_LIST:~1%"

if "%OPT_OUTDIR%"=="" set "OPT_OUTDIR=%ROOT_DIR%\build"
call :read_version "%ROOT_DIR%\server\internal\version\version.go" "1.0.1"
if "%OPT_SERVER_VERSION%"=="" set "OPT_SERVER_VERSION=%RSV%"
call :read_version "%ROOT_DIR%\agent\internal\version\version.go" "1.0.1"
if "%OPT_AGENT_VERSION%"=="" set "OPT_AGENT_VERSION=%RSV%"
if "%MOCENTER_SERVER_VERSION%"=="" set "MOCENTER_SERVER_VERSION=%OPT_SERVER_VERSION%"
if "%MOCENTER_AGENT_VERSION%"==""  set "MOCENTER_AGENT_VERSION=%OPT_AGENT_VERSION%"

set "SERVER_BIN=%OPT_OUTDIR%\windows\binaries\server\windows"
set "AGENT_BIN=%OPT_OUTDIR%\windows\binaries\agent\windows"
set "WEB_OUT=%OPT_OUTDIR%\windows\web"
set "ZIP_OUT=%OPT_OUTDIR%\windows\packages\zip"
set "SETUP_OUT=%OPT_OUTDIR%\windows\installer"

echo ==========================================
echo  MoCenter build (Windows)
echo  Root:      %ROOT_DIR%
echo  Output:    %OPT_OUTDIR%
echo  Type:      %OPT_TYPE%
echo  Packages:  %OPT_PACKAGE%
echo  Arch:      %ARCH_LIST%
echo  Binary:    %OPT_BINARY%
echo  Web:       %OPT_WEB%
echo  Versions:  server %OPT_SERVER_VERSION% / agent %OPT_AGENT_VERSION%
echo ==========================================

if not exist "%OPT_OUTDIR%" mkdir "%OPT_OUTDIR%"
if "%OPT_CLEAN%"=="1" (
  echo Cleaning %OPT_OUTDIR% ...
  if exist "%OPT_OUTDIR%\windows" rmdir /s /q "%OPT_OUTDIR%\windows"
) else (
  REM Remove only what this run will regenerate.  --binary=0 means "reuse the
  REM existing binaries", so those must be kept -- they are the packaging input.
  if "%OPT_BINARY%"=="1" (
    if "%WANT_SERVER%"=="1" if exist "%SERVER_BIN%" rmdir /s /q "%SERVER_BIN%"
    if "%WANT_AGENT%"=="1"  if exist "%AGENT_BIN%"  rmdir /s /q "%AGENT_BIN%"
  )
  if "%WANT_WEB%"=="1" if exist "%WEB_OUT%" rmdir /s /q "%WEB_OUT%"
  if /i "%OPT_PACKAGE%"=="all" if exist "%ZIP_OUT%" rmdir /s /q "%ZIP_OUT%"
  if /i "%OPT_PACKAGE%"=="zip" if exist "%ZIP_OUT%" rmdir /s /q "%ZIP_OUT%"
  if "%WANT_INSTALLER%"=="1" if exist "%SETUP_OUT%" rmdir /s /q "%SETUP_OUT%"
)

REM ---------------------------------------------------------------- server
if "%WANT_SERVER%%WANT_AGENT%"=="11" goto check_go
if "%WANT_SERVER%%WANT_AGENT%"=="10" goto check_go
if "%WANT_SERVER%%WANT_AGENT%"=="01" goto check_go
goto skip_go
:check_go
if "%OPT_BINARY%"=="0" goto skip_go
where go >nul 2>nul
if errorlevel 1 (
  echo ** Go not found. Install Go 1.25+ and add it to PATH.
  exit /b 1
)
:skip_go

if "%WANT_SERVER%"=="1" if "%OPT_BINARY%"=="1" (
  echo(
  echo ^>^>^> Server binaries ^(version %OPT_SERVER_VERSION%^)
  call :build_one "%ROOT_DIR%\server" amd64 "%SERVER_BIN%\mocenter-server-amd64.exe" ""
  if errorlevel 1 exit /b 1
)
if "%WANT_SERVER%"=="1" if "%OPT_BINARY%"=="0" echo   --binary=0, reusing existing server binaries

REM ---------------------------------------------------------------- agent
if "%WANT_AGENT%"=="1" if "%OPT_BINARY%"=="1" (
  echo(
  echo ^>^>^> Agent binaries ^(version %OPT_AGENT_VERSION%^)
  for %%A in (%ARCH_LIST%) do (
    call :build_one "%ROOT_DIR%\agent" %%A "%AGENT_BIN%\mocenter-agent-%%A.exe" "-X mocenter/agent/internal/version.Version=%OPT_AGENT_VERSION%"
    if errorlevel 1 exit /b 1
  )
)
if "%WANT_AGENT%"=="1" if "%OPT_BINARY%"=="0" echo   --binary=0, reusing existing agent binaries

REM ---------------------------------------------------------------- web
if "%WANT_WEB%"=="1" (
  echo(
  echo ^>^>^> Web frontend
  if "%OPT_WEB%"=="0" (
    echo   --web=0, skipping web build
  ) else (
    call :build_web
    if errorlevel 1 exit /b 1
  )
)

REM ---------------------------------------------------------------- zip packages
set "DO_ZIP=0"
if /i "%OPT_PACKAGE%"=="all" set "DO_ZIP=1"
if /i "%OPT_PACKAGE%"=="zip" set "DO_ZIP=1"
if "%WANT_AGENT%%WANT_SERVER%"=="00" set "DO_ZIP=0"
if "%DO_ZIP%"=="1" (
  echo(
  echo ^>^>^> Portable zip packages
  if "%WANT_AGENT%"=="1" for %%A in (%ARCH_LIST%) do call :zip_role agent "%%A"
  if "%WANT_SERVER%"=="1" for %%A in (%ARCH_LIST%) do (
    if "%%A"=="amd64" call :zip_role server "%%A"
    if "%%A"=="386"   call :zip_role server "%%A"
  )
)

REM ---------------------------------------------------------------- installer
if "%WANT_INSTALLER%"=="1" (
  echo(
  echo ^>^>^> Windows GUI setup ^(Inno Setup^)
  if exist "%ROOT_DIR%\installer\setup.iss" (
    call "%ROOT_DIR%\scripts\build-windows-installer.bat" "%OPT_OUTDIR%" "%OPT_AGENT_VERSION%"
    if errorlevel 1 exit /b 1
  ) else (
    echo   ** installer\setup.iss not found, skipping
  )
)

echo(
echo ==========================================
echo  Build finished
echo ==========================================
if exist "%OPT_OUTDIR%" for /f "delims=" %%F in ('dir /b /s "%OPT_OUTDIR%" 2^>nul') do echo   %%~nxF  ^(%%~zF bytes^)
echo.
echo  Next steps:
echo    - Register a service on a target host ^(as admin^):
echo        unzip build\windows\packages\zip\*.zip then run install-windows.bat install ^<server^>
echo    - GUI installer output: build\windows\installer\MoCenter-Setup-*.exe
echo ==========================================
endlocal
exit /b 0

REM =============================================================
REM EVERYTHING BELOW IS A SUBROUTINE.  Do not place subroutines above the
REM "exit /b 0" line: cmd executes them as part of the main flow.
REM =============================================================

REM :split_pair <key> <value>
REM Called with the "--name<space>value" form produced by rewriting "=" to a
REM space.  Anything after a further "=" stays in %2 because cmd only splits on
REM the separators that are actually present in the command line.
:split_pair
set "OPT_KEY=%~1"
if "%~2"=="" ( set "OPT_VALUE=" ) else ( set "OPT_VALUE=%~2" )
exit /b 0

REM :read_version <go source file> <fallback version>
REM Reads `var Version = "x.y.z"` (or `const Version = ...`) into RSV.
:read_version
set "RV_FILE=%~1"
set "RV_FALLBACK=%~2"
set "RSV=%RV_FALLBACK%"
for /f "tokens=1,2,3" %%A in ('findstr /r /c:"^var Version" /c:"^const Version" "%RV_FILE%" 2^>nul') do call :rv_take %%C
exit /b 0

:rv_take
if "%~1"=="" exit /b 0
set "V=%~1"
set "V=%V:"=%"
if not "%V%"=="" set "RSV=%V%"
exit /b 0

REM :build_one <module dir> <goarch> <output file> [extra ldflags]
REM
REM Self-healing module cache: a truncated entry in %GOMODCACHE% makes go print
REM "zip: not a valid zip file" and then fail EVERY import resolved from that
REM module (looks like a broken source tree, is in fact a bad local download),
REM so the cache is cleared and the download retried through a mirror proxy.
REM GOFLAGS=-mod=mod is deliberately NOT set: go.sum stays the source of truth
REM for third-party code in a remote-control product.
:build_one
set "MOD_DIR=%~1"
set "MOD_ARCH=%~2"
set "MOD_OUT=%~3"
set "MOD_LD=%~4"
set "GOOS=windows"
set "GOARCH=%MOD_ARCH%"
set "CGO_ENABLED=0"
REM GOARM is not set on purpose: windows/arm is rejected during argument parsing,
REM so only amd64/386/arm64 ever reach here.
if not exist "%~dp3" mkdir "%~dp3"
echo   [go] %MOD_OUT% (GOOS=%GOOS% GOARCH=%GOARCH%)
pushd "%MOD_DIR%"
go mod download
if errorlevel 1 (
  echo   !! go mod download failed - clearing module cache and retrying via mirror
  go clean -modcache
  set "GOPROXY=https://goproxy.cn,direct"
  go mod download
  if errorlevel 1 (
    popd
    echo   ** module download still failing. Check the network or set GOPROXY.
    exit /b 1
  )
)
go build -ldflags "-s -w %MOD_LD%" -o "%MOD_OUT%" .
set "BUILD_RC=%errorlevel%"
if not "%BUILD_RC%"=="0" (
  echo   !! build failed - clearing module cache and retrying via mirror
  go clean -modcache
  set "GOPROXY=https://goproxy.cn,direct"
  go build -ldflags "-s -w %MOD_LD%" -o "%MOD_OUT%" .
  set "BUILD_RC=%errorlevel%"
)
popd
if not "%BUILD_RC%"=="0" (
  echo   ** build failed for %MOD_DIR%
  exit /b 1
)
echo   -^> %MOD_OUT%
exit /b 0

REM :build_web -- npm install + build, then copy dist into the outdir
:build_web
if exist "%ROOT_DIR%\web\dist\index.html" if not "%OPT_WEB%"=="1" (
  echo   reusing existing web\dist ^(use --web=1 or --clean to rebuild^)
  goto :copy_web
)
where npm >nul 2>nul
if errorlevel 1 (
  echo ** npm not found but --type includes web. Install Node.js 18+,
  echo    or drop web from --type / pass --web=0.
  exit /b 1
)
pushd "%ROOT_DIR%\web"
call npm install --no-audit --no-fund
call npm run build
popd
:copy_web
if not exist "%ROOT_DIR%\web\dist\index.html" (
  echo ** web\dist\index.html missing - frontend build produced nothing
  exit /b 1
)
if not exist "%WEB_OUT%" mkdir "%WEB_OUT%"
xcopy /e /i /q /y "%ROOT_DIR%\web\dist" "%WEB_OUT%" >nul
echo   -^> %WEB_OUT%
exit /b 0

REM :zip_role <role> <arch>
REM Assembles a stage dir (bin\ + config templates + service scripts) and zips
REM it with PowerShell Compress-Archive -- no extra tooling needed.
:zip_role
set "ZR_ROLE=%~1"
set "ZR_ARCH=%~2"
if /i "%ZR_ROLE%"=="server" ( set "ZR_BIN=%SERVER_BIN%\mocenter-server-%ZR_ARCH%.exe" ) else ( set "ZR_BIN=%AGENT_BIN%\mocenter-agent-%ZR_ARCH%.exe" )
if not exist "%ZR_BIN%" (
  echo   ** missing %ZR_BIN%, skipping zip for %ZR_ROLE%-%ZR_ARCH%
  echo      build it first: scripts\build-all.bat --type=%ZR_ROLE% --arch=%ZR_ARCH% --package=none
  exit /b 0
)
set "ZR_STAGE=%OPT_OUTDIR%\windows\packages\.stage-%ZR_ROLE%-%ZR_ARCH%"
if exist "%ZR_STAGE%" rmdir /s /q "%ZR_STAGE%"
mkdir "%ZR_STAGE%\bin"
copy /y "%ZR_BIN%" "%ZR_STAGE%\bin\" >nul
if exist "%ROOT_DIR%\scripts\deploy\mocenter-%ZR_ROLE%.conf" copy /y "%ROOT_DIR%\scripts\deploy\mocenter-%ZR_ROLE%.conf" "%ZR_STAGE%\bin\" >nul
if exist "%ROOT_DIR%\scripts\deploy\install-windows.bat"   copy /y "%ROOT_DIR%\scripts\deploy\install-windows.bat"   "%ZR_STAGE%\" >nul
if exist "%ROOT_DIR%\scripts\deploy\uninstall-windows.bat" copy /y "%ROOT_DIR%\scripts\deploy\uninstall-windows.bat" "%ZR_STAGE%\" >nul
(
  echo MoCenter %ZR_ROLE% %ZR_ARCH% portable package
  echo =========================================
  echo   bin\mocenter-%ZR_ROLE%.exe     main program ^(portable: config lands next to it^)
  echo   bin\mocenter-%ZR_ROLE%.conf    config template
  echo   install-windows.bat           register as a Windows service ^(admin^)
  echo   uninstall-windows.bat         remove the service
  echo.
  echo Register as a service:
  echo   install-windows.bat install ^<server^> [agentid] [proxy]
) > "%ZR_STAGE%\README.txt"
if not exist "%ZIP_OUT%" mkdir "%ZIP_OUT%"
set "ZR_ZIP=%ZIP_OUT%\mocenter-%ZR_ROLE%-%OPT_SERVER_VERSION%-windows-%ZR_ARCH%.zip"
if exist "%ZR_ZIP%" del /q "%ZR_ZIP%"
powershell -NoProfile -ExecutionPolicy Bypass -Command "Compress-Archive -Path '%ZR_STAGE%\*' -DestinationPath '%ZR_ZIP%' -Force" >nul
if not exist "%ZR_ZIP%" (
  echo   ** zip creation failed for %ZR_ROLE%-%ZR_ARCH%
  rmdir /s /q "%ZR_STAGE%"
  exit /b 1
)
REM Verify the archive is actually readable, not merely present on disk
powershell -NoProfile -ExecutionPolicy Bypass -Command "Add-Type -AssemblyName System.IO.Compression.FileSystem; $z=[IO.Compression.ZipFile]::OpenRead('%ZR_ZIP%'); $n=$z.Entries.Count; $z.Dispose(); if($n -lt 3){ exit 1 }" >nul
if errorlevel 1 (
  echo   ** zip integrity check failed: %ZR_ZIP%
  rmdir /s /q "%ZR_STAGE%"
  exit /b 1
)
rmdir /s /q "%ZR_STAGE%"
echo   -^> %ZR_ZIP%  ^(verified^)
exit /b 0

:show_list
echo Allowed values on Windows:
echo   --type      agent ^| server ^| web ^| installer ^| none ^| all
echo   --package   zip ^| none ^| all
echo   --platform  windows ^| all
echo   --arch      amd64 ^| 386 ^| arm64 ^| all
echo   --binary    1 ^| 0
echo   --web       1 ^| 0 ^| auto
echo.
echo Linux artifacts (.deb/.rpm/Linux binaries) are built by
echo   bash scripts/build-linux-all.sh
echo on a Linux/macOS host; this script does not cross-build them.
exit /b 0

:show_help
echo MoCenter unified build script ^(Windows^)
echo.
echo Usage: scripts\build-all.bat [options]
echo.
echo   scripts\build-all.bat                        all types, all arches
echo   scripts\build-all.bat --type=agent
echo   scripts\build-all.bat --type=server --arch=amd64
echo   scripts\build-all.bat --type=agent --package=zip --arch=amd64,386
echo   scripts\build-all.bat --type=installer
echo   scripts\build-all.bat --web=0 --package=none
echo   scripts\build-all.bat --outdir=D:\mocenter-build
echo   scripts\build-all.bat --list
echo.
echo Options:
echo   --type=     agent ^| server ^| web ^| installer ^| none ^| all
echo   --package=  zip ^| none ^| all        ^(Windows side produces zip^)
echo   --platform= windows ^| all           ^(Linux targets: build-linux-all.sh^)
echo   --arch=     amd64 ^| 386 ^| arm64 ^| all
echo   --binary=   1 ^| 0
echo   --web=      1 ^| 0 ^| auto
echo   --outdir=   directory                default ^<repo^>\build
echo   --version=  server version           default read from server source
echo   --agent-version= agent version       default read from agent source
echo   --clean     wipe outdir first
echo   --list      print allowed values
echo.
echo Artifacts:
echo   build\windows\binaries\server\windows\mocenter-server-^<arch^>.exe
echo   build\windows\binaries\agent\windows\mocenter-agent-^<arch^>.exe
echo   build\windows\web\                       web frontend static files
echo   build\windows\packages\zip\              portable zips ^(binary+scripts^)
echo   build\windows\installer\                 GUI setup ^(Inno Setup^)
exit /b 0
