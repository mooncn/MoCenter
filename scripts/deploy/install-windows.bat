@echo off
REM =============================================================
REM MoCenter Windows 服务注册脚本（需管理员）
REM
REM 用法:
REM   install-windows.bat install <server> [agentid] [proxy]
REM   install-windows.bat install-server [user] [listen] [http] [--password-file=<路径>]
REM   install-windows.bat status | uninstall
REM
REM 示例:
REM   install-windows.bat install mc.example.com:9000
REM   install-windows.bat install mc.example.com:9000 host-01
REM   install-windows.bat install-server admin :9000 :8080
REM   install-windows.bat install-server admin :9000 :8080 --password-file=C:\tmp\pw.txt
REM   type C:\tmp\pw.txt | install-windows.bat install-server admin
REM
REM 说明:
REM   · 优先使用图形安装程序或 mocenter-deploy.exe（走 SCM 握手，服务状态可被
REM     sc query / 服务管理器正确观察）；本脚本是直接用 sc create 的备选路径。
REM   · 二进制默认取自本脚本同目录（免安装包解压后即为该布局）：
REM       <同目录>\bin\mocenter-<role>.exe  或  <同目录>\mocenter-<role>.exe
REM
REM ⚠️ 关于口令（重要）:
REM   Windows 服务的 binPath 记录在注册表 HKLM\SYSTEM\CurrentControlSet\Services\<名>\ImagePath，
REM   **任何能读注册表的本机用户都能看到**，`sc qc MoCenterServer` 也能直接打印出来。
REM   同理，**调用本脚本的那条命令行**也对同机任意进程可见（进程列表"命令行"列），
REM   所以口令不再收位置参数，改走以下来源（按优先级）:
REM     1. --password-file=<路径>   复制到 %ProgramData%\MoCenter\initial-password；
REM     2. MOCENTER_PASSWORD 环境变量；
REM     3. 标准输入一行（type pw.txt | install-windows.bat install-server admin）；
REM     4. 都没有则交互输入（会回显），直接回车 = 自动生成随机强口令。
REM   因此本脚本：
REM     · Agent：binPath 只给 -config，服务端地址/标识全部放进收紧权限的配置文件；
REM     · Server：binPath 只给 -password-file，口令落在仅管理员可读的文件里，
REM       服务端启动时读取它创建初始账号（已有账号后该文件即失效）。
REM   这样即使服务定义被读走，也拿不到口令。
REM =============================================================
@setlocal enabledelayedexpansion

set "ACTION=%~1"
set "SERVER=%~2"
set "AGENT_ID=%~3"
set "PROXY_IP=%~4"
set "INSTALL_DIR=%ProgramFiles%\MoCenter"
set "HERE=%~dp0"
if "%HERE:~-1%"=="\" set "HERE=%HERE:~0,-1%"

if "%ACTION%"=="" goto usage
if /i "%ACTION%"=="status" goto status
if /i "%ACTION%"=="uninstall" goto uninstall
if /i "%ACTION%"=="install" goto install_agent
if /i "%ACTION%"=="install-server" goto install_server
if /i "%ACTION%"=="install-agent" goto install_agent

echo 未知操作: %ACTION%
goto usage

REM ---------------------------------------------------------------- Agent
:install_agent
if "%SERVER%"=="" (
  echo 请指定服务端地址，如: install-windows.bat install mc.example.com:9000
  exit /b 1
)
call :require_admin || exit /b 1
call :find_bin agent || exit /b 1

echo [1/4] 复制 Agent 二进制到 %INSTALL_DIR% ...
call :ensure_dir "%INSTALL_DIR%"
copy /y "%BIN_SRC%" "%INSTALL_DIR%\mocenter-agent.exe" >nul

echo [2/4] 生成配置文件 ...
REM Agent 的 -config 读的是 **JSON**（agent/internal/config），不是 KEY=VALUE。
REM 这里显式写 JSON；同时把权限收紧到仅 SYSTEM 与管理员可读
REM （配置里可能含 agent_key / 注册密钥）。
call :write_agent_conf "%INSTALL_DIR%\mocenter-agent.conf"

echo [3/4] 注册服务 MoCenterAgent ...
sc stop MoCenterAgent >nul 2>&1
sc delete MoCenterAgent >nul 2>&1
REM 程序通过 svc.IsWindowsService() 自动识别被 SCM 拉起，不需要 NSSM，也不需要 -daemon
REM 只传 -config：服务端地址/标识都从配置文件读。
REM 原因见文件头 —— binPath 会完整写进注册表 ImagePath，内联参数等于公开配置；
REM 更糟的是，一旦参数写进 binPath，之后改配置文件**不会生效**（命令行优先级更高），
REM 运维会以为改了却一直连旧地址。
sc create MoCenterAgent binPath= "\"%INSTALL_DIR%\mocenter-agent.exe\" -config \"%INSTALL_DIR%\mocenter-agent.conf\"" start= auto DisplayName= "MoCenter Agent" >nul
if errorlevel 1 ( echo   服务创建失败，请确认以管理员运行 & exit /b 1 )

echo [4/4] 启动服务 ...
sc failure MoCenterAgent reset= 86400 actions= restart/5000/restart/5000/restart/5000 >nul 2>&1
sc start MoCenterAgent >nul 2>&1
echo.
echo OK - MoCenter Agent 已注册为系统服务（开机自启）
echo   服务名: MoCenterAgent
echo   二进制: %INSTALL_DIR%\mocenter-agent.exe
echo   配置:   %INSTALL_DIR%\mocenter-agent.conf
echo   服务端: %SERVER%
goto end

REM ---------------------------------------------------------------- Server
:install_server
call :require_admin || exit /b 1
set "S_USER=%~2"
set "S_LISTEN=%~3"
set "S_HTTP=%~4"
set "S_PW_SRC="
set "S_PW_GENERATED="
set "S_PASS="
call :scan_args %* || exit /b 1
REM 旧版把口令收在第 3 个位置参数上（install-server <user> <password> ...）。
REM 位置式口令已移除：调用方命令行对同机任意进程可见，等于当众广播口令。
REM 监听地址必含冒号（":9000" / "0.0.0.0:9000"），据此识别旧写法并明确报错，
REM 不静默把口令当成监听地址装出一个连不上的服务。
if not "%S_LISTEN%"=="" if "%S_LISTEN::=%"=="%S_LISTEN%" (
  echo 检测到旧版"位置式口令"写法：第 3 个参数 "%S_LISTEN%" 不是监听地址（应含冒号，如 :9000^）。
  echo   口令已不允许走命令行 —— 那会在同机进程列表里可见。可选方式:
  echo     省略口令: install-server %S_USER% :9000 :8080        ^(自动生成随机强口令^)
  echo     指定文件: install-server %S_USER% :9000 :8080 --password-file=^<路径^>
  echo     标准输入: type pw.txt ^| install-windows.bat install-server %S_USER%
  exit /b 1
)
if "%S_USER%"=="" set "S_USER=admin"
REM 不再默认 change-me：固定默认口令写在脚本里等于公开口令，"忘了改"就是一条现成入侵路径。
REM 口令来源与优先级见文件头；留空 = 安装时生成随机强口令（见 :write_password_file）。
if "%S_LISTEN%"=="" set "S_LISTEN=:9000"
if "%S_HTTP%"=="" set "S_HTTP=:8080"
call :find_bin server || exit /b 1

echo [1/4] 复制 Server 二进制到 %INSTALL_DIR% ...
call :ensure_dir "%INSTALL_DIR%"
copy /y "%BIN_SRC%" "%INSTALL_DIR%\mocenter-server.exe" >nul

REM 口令落盘为仅管理员可读的文件，binPath 只引用它 —— 不再把口令写进服务定义
set "S_PW_FILE=%ProgramData%\MoCenter\initial-password"
call :write_password_file "%S_PW_FILE%" || exit /b 1

echo [3/4] 注册服务 MoCenterServer ...
sc stop MoCenterServer >nul 2>&1
sc delete MoCenterServer >nul 2>&1
sc create MoCenterServer binPath= "\"%INSTALL_DIR%\mocenter-server.exe\" -listen %S_LISTEN% -http %S_HTTP% -user %S_USER% -password-file \"%S_PW_FILE%\" -data \"%ProgramData%\MoCenter\server\"" start= auto DisplayName= "MoCenter Server" >nul
if errorlevel 1 ( echo   服务创建失败，请确认以管理员运行 & exit /b 1 )

echo [4/4] 启动服务 ...
sc failure MoCenterServer reset= 86400 actions= restart/5000/restart/5000/restart/5000 >nul 2>&1
sc start MoCenterServer >nul 2>&1
echo.
echo OK - MoCenter Server 已注册为系统服务（开机自启）
echo   后台账号: %S_USER%  ^(请登录后立即修改口令^)
echo   口令文件: %S_PW_FILE%  ^(仅 Administrators 可读；建好账号后可删除^)
echo   监听: %S_LISTEN% ^(Agent^) / %S_HTTP% ^(Web 控制台^)
echo   安全说明: 口令不会出现在服务定义里，可用 sc qc MoCenterServer 自行核对
goto end

REM ---------------------------------------------------------------- 公共子过程

REM :write_password_file —— 生成仅管理员可读的初始口令文件
REM
REM 与 Linux 侧同理：口令绝不能进 binPath（注册表可见），也不能进调用方命令行。
REM 口令来源与优先级见文件头。已存在任何账号时服务端会忽略该文件内容，
REM 因此重复安装不会把口令重置回去；但对**已存在的文件不覆盖** ——
REM 宁可让运维手动删，也不冒覆盖新口令的风险。
REM
REM 结构说明（刻意用 goto 而不是 if/else 括号块）：
REM   cmd 对整个括号块**先整体解析、后执行**，块内的 %VAR% 在块解析时就展开成
REM   当时的值 —— 旧版在 for /f 生成随机口令之后用 %S_PASS% 判断，展开的永远是
REM   空串，"已生成初始口令（仅本次显示）"那条提示**从未显示过**。goto 流程
REM   每行独立解析，%S_PASS% 才是刚赋的值；S_PASS 的值里也可能含 ! 等特殊字符，
REM   故一律不用延迟展开。
:write_password_file
set "PW_FILE=%~1"
if "%PW_FILE%"=="" ( echo   内部错误: 未指定口令文件路径 & exit /b 1 )
if exist "%PW_FILE%" (
  echo   保留现有口令文件: %PW_FILE% ^(已登录过的话该文件可安全删除^)
  exit /b 0
)
call :ensure_dir "%ProgramData%\MoCenter"

REM 来源 1：--password-file=<路径>（运维已备好的口令文件，复制到固定位置）
if defined S_PW_SRC goto pw_from_src

REM 来源 2：MOCENTER_PASSWORD 环境变量（与 Linux 侧一致）
if not defined S_PASS if defined MOCENTER_PASSWORD set "S_PASS=%MOCENTER_PASSWORD%"

REM 来源 3：标准输入读一行。管道给入（type pw.txt | 本脚本）不回显；
REM 交互输入会回显在屏幕上，介意请用前两种方式。空输入/无输入继续往下走随机生成。
if not defined S_PASS set /p "S_PASS=请输入初始口令（直接回车 = 自动生成随机强口令）: "
if defined S_PASS goto pw_have

REM 来源 4：自动生成随机强口令（纯 cmd 没有可靠的随机源，用 PowerShell）。
REM 两个坑：
REM   1. `for /f` 的反引号里 **不能再出现 % 或 %%**（会被 cmd 当变量展开先处理掉），
REM      所以这里用 PowerShell 的 -join/数组字面量写法，不写 %%{ } 循环；
REM   2. 只取字母数字，避免 $ ` " ^ ! 等字符在后续 shell/配置里被二次解析
REM      （! 尤其要避开：本脚本开着延迟展开，值里的 ! 会被吃掉）。
REM      字符集用 .Length 取长度，不写死数字 —— 写死很容易和实际字符数差一，
REM      结果是某个字符永远选不到（口令空间白白变小）。
REM      注意 `|` 在 for /f 的反引号里必须写成 `^|` 才不会在此之前被当成管道。
for /f "usebackq delims=" %%p in (`powershell -NoProfile -Command "$cs='ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'; -join (1..16 ^| ForEach-Object { $cs[(Get-Random -Maximum $cs.Length)] })"`) do set "S_PASS=%%p"
if not defined S_PASS (
  echo   无法生成随机口令，请用 --password-file=^<路径^> 指定口令
  exit /b 1
)
set "S_PW_GENERATED=1"
:pw_have
REM 注意：>/ 而不是 >，避免在文件尾部写入多余的空行（口令必须逐字节精确）
> "%PW_FILE%" echo|set /p="%S_PASS%"
goto pw_acl
:pw_from_src
if not exist "!S_PW_SRC!" (
  echo   指定的口令文件不存在: !S_PW_SRC!
  exit /b 1
)
copy /y "!S_PW_SRC!" "%PW_FILE%" >nul
if errorlevel 1 (
  echo   复制口令文件失败: !S_PW_SRC! -^> %PW_FILE%
  exit /b 1
)
echo   已使用指定的口令文件: !S_PW_SRC! -^> %PW_FILE%
:pw_acl
REM 权限收紧到 SYSTEM + Administrators，等价于 Linux 的 0600
icacls "%PW_FILE%" /inheritance:r /grant:r "*S-1-5-18:(R)" "*S-1-5-32-544:(F)" >nul 2>&1
if errorlevel 1 (
  echo   警告: 收紧 %PW_FILE% 权限失败，请手动确认该文件不可被普通用户读取
)
if defined S_PW_GENERATED (
  echo.
  echo   ^>^>^> 已生成初始口令（仅本次显示）: %S_PASS%
  echo       写入: %PW_FILE%
  echo       提示: 登录后请立即在「账号设置」修改密码，随后删除该文件。
) else (
  echo   已写入初始口令文件: %PW_FILE%
)
exit /b 0

REM :scan_args —— 在全部参数里找 --password-file=<路径>（位置无关）。
REM 路径值可能含空格与括号，故取出后一律用延迟展开（!S_PW_SRC!）消费，
REM 避免值里的括号在括号块解析时被当成块定界符。
:scan_args
set "S_PW_SRC="
:scan_args_loop
if "%~1"=="" exit /b 0
set "SCAN_ARG=%~1"
if /i "!SCAN_ARG:~0,16!"=="--password-file=" set "S_PW_SRC=!SCAN_ARG:~16!"
shift
goto scan_args_loop

REM :write_agent_conf —— 生成 Agent 的 JSON 配置
REM
REM Agent 的 -config 读的是 JSON（agent/internal/config），不是 KEY=VALUE。
REM 先用临时文件拼装、再 icacls 收紧后落地：避免"先建出人人可读的文件、再补权限"
REM 中间那段时间窗口里密钥（agent_key）可被读走。
:write_agent_conf
set "CONF_DEST=%~1"
set "CONF_TMP=%TEMP%\mocenter-agent-conf-%RANDOM%.json"
> "%CONF_TMP%" (
  echo {
  echo   "server": "%SERVER%",
  if not "%AGENT_ID%"=="" echo   "agent_id": "%AGENT_ID%",
  if not "%PROXY_IP%"=="" echo   "proxy_ip": "%PROXY_IP%",
  echo   "heartbeat": 30,
  echo   "probe_every": 10,
  echo   "daemon": true
  echo }
)
move /y "%CONF_TMP%" "%CONF_DEST%" >nul
icacls "%CONF_DEST%" /inheritance:r /grant:r "*S-1-5-18:(F)" "*S-1-5-32-544:(F)" >nul 2>&1
exit /b 0

:require_admin
net session >nul 2>&1
if errorlevel 1 (
  echo 请以管理员身份运行此脚本（右键 - 以管理员身份运行）
  exit /b 1
)
exit /b 0

:ensure_dir
if not exist "%~1" mkdir "%~1"
exit /b 0

REM find_bin <role> —— 依次在本目录、bin\ 子目录中查找二进制
:find_bin
set "BIN_SRC="
for %%r in (%~1) do (
  if exist "%HERE%\mocenter-%%r.exe" set "BIN_SRC=%HERE%\mocenter-%%r.exe"
  if not defined BIN_SRC if exist "%HERE%\bin\mocenter-%%r.exe" set "BIN_SRC=%HERE%\bin\mocenter-%%r.exe"
  if not defined BIN_SRC (
    echo 未找到 mocenter-%%r.exe，请把它放在本脚本同目录或其 bin\ 子目录中
    exit /b 1
  )
)
echo   使用二进制: %BIN_SRC%
exit /b 0

:status
sc query MoCenterAgent 2>nul | findstr /i "SERVICE_NAME STATE" || echo 服务 MoCenterAgent 未安装
sc query MoCenterServer 2>nul | findstr /i "SERVICE_NAME STATE" || echo 服务 MoCenterServer 未安装
goto end

:uninstall
sc stop MoCenterAgent >nul 2>&1
sc delete MoCenterAgent >nul 2>&1
sc stop MoCenterServer >nul 2>&1
sc delete MoCenterServer >nul 2>&1
echo 已删除服务 MoCenterAgent / MoCenterServer
echo 程序目录 %INSTALL_DIR% 与数据目录 %ProgramData%\MoCenter 已保留，如需彻底清理请手动删除
goto end

:usage
echo 用法:
echo   install-windows.bat install ^<server^> [agentid] [proxy]
echo   install-windows.bat install-server [user] [listen] [http] [--password-file=^<路径^>]
echo   install-windows.bat status ^| uninstall
echo.
echo 口令来源（Server 安装）: --password-file=^<路径^> ^| MOCENTER_PASSWORD 环境变量
echo   ^| 标准输入一行 ^| 交互输入 ^| 留空自动生成随机强口令
echo   口令不允许走命令行参数 —— 调用方命令行对同机任意进程可见。
goto end

:end
endlocal
