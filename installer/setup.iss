; =============================================================
; MoCenter Windows 图形安装向导（Inno Setup 6）
;
; 构建前置：
;   1) 运行 scripts\build-all.bat --type=installer
;      —— 它会编译 agent / server / 安装引擎到 installer\payload\，再调用本脚本
;   2) 编译本脚本：
;        "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" installer\setup.iss
;      产物：build\setup\MoCenter-Setup-<版本>.exe
;
; 设计说明：
;   - 向导只负责「收集参数 + 摆放文件」，真正的服务注册、配置落盘、
;     单实例与日志重定向仍由 mocenter-deploy.exe 完成，避免两套逻辑各写一遍。
;   - 因此本脚本里没有任何服务注册代码，只有界面与参数拼装。
;   - 向导**不**做「全新安装 / 升级」的区分：引擎会按"配置是否存在"自动判断 ——
;     已装过的机器上重跑向导只会替换二进制与服务，配置与账号原样保留。
;     需要清理重装时用命令行引擎加 -force。
;   - 角色页决定后续显示哪些参数页：只装 Agent 时不出现 Server 的账号/端口，
;     只装 Server 时不出现 Agent 的连接/令牌（ShouldSkipPage）。
;   - 卸载时会询问是否连同配置与数据一起删除（默认保留）。静默卸载可用
;     `unins000.exe /SILENT /PURGE` 显式要求清除；数据目录来自安装时写入
;     注册表 HKLM\Software\MoCenter\DataDir，自定义目录也能删对地方。
;
; 关于中文界面：
;   向导自定义页面（标题、说明、字段）全部是中文；框架按钮（下一步/安装等）
;   的文案覆盖放在 messages-cn.iss，若 ISCC 报语言前缀错误，只需改那一个文件。
; =============================================================

#define AppName "MoCenter"
; 版本号在本文件与 Go 源码 internal/version 中**必须一致**。
; 发布纪律：只在明确发版时修改，且 server / agent 共用同一版本号。
; 一致性由 server/internal/version 的单元测试守护（含对 agent 源码的比对）。
#define AppVersion "1.0.4"
#define AppPublisher "MoCenter"

[Setup]
; AppId 必须固定：升级与卸载都靠它识别同一个产品（花括号需写成 {{ 转义）
AppId={{7C4E1A62-9D53-4B18-8F27-3A6C5E9D4B70}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
DefaultDirName={autopf}\MoCenter
DisableProgramGroupPage=yes
; 注册服务、写 Program Files 都需要管理员
PrivilegesRequired=admin
OutputDir=..\build\setup
OutputBaseFilename=MoCenter-Setup-{#AppVersion}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; Go 1.21+ 编译的二进制最低要求 Windows 10
MinVersion=10.0
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\mocenter-deploy.exe

#include "messages-cn.iss"

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "payload\mocenter-agent.exe";  DestDir: "{app}"; Flags: ignoreversion
Source: "payload\mocenter-server.exe"; DestDir: "{app}"; Flags: ignoreversion
; 安装引擎：既被本向导调用，也可单独用于静默/脚本化部署
Source: "payload\mocenter-deploy.exe"; DestDir: "{app}"; Flags: ignoreversion
; Web 前端（若已构建）：交给 Server 托管，装完即可直接打开控制台
Source: "payload\web\*"; DestDir: "{app}\web"; \
  Flags: ignoreversion recursesubdirs createallsubdirs skipifsourcedoesntexist

[Registry]
; 记录安装时选择的角色：卸载阶段没有向导状态，靠它决定要移除哪些服务
Root: HKLM; Subkey: "Software\MoCenter"; ValueType: string; ValueName: "Role"; \
  ValueData: "{code:GetRoleStr}"; Flags: uninsdeletekey
; 记录数据目录：用户可能把数据放到非默认位置，卸载时按同一个目录清理/保留
Root: HKLM; Subkey: "Software\MoCenter"; ValueType: string; ValueName: "DataDir"; \
  ValueData: "{code:GetDataDirStr}"; Flags: uninsdeletekey

; 卸载动作不走 [UninstallRun]：该节参数在安装阶段就求值并写入卸载日志，
; 无法携带"卸载时才询问"的 -purge 选择。改由 CurUninstallStepChanged(usUninstall)
; 直接调用 mocenter-deploy.exe（见 [Code] 末尾）。

[Code]
var
  PageRole: TInputOptionWizardPage;
  PageAgent: TInputQueryWizardPage;      // Agent 专用：服务端地址 / 名称 ID / 注册凭据
  PageServer: TInputQueryWizardPage;     // Server 专用：后台用户名 / 口令
  PageAdv: TInputQueryWizardPage;        // 公共：数据目录 / 代理 / 额外 TLS 参数
  PageAdvServer: TInputQueryWizardPage;  // Server 专用：Agent 与 HTTP 监听
  PageLog: TInputOptionWizardPage;       // 公共：运行期日志级别（Agent/Server 各写各的配置）
  PurgeData: Boolean;                    // 卸载时是否删除配置与数据

// SafeVal 清掉值里的双引号并去空白 —— 这些值要拼进命令行，带引号会破坏参数边界。
// 注意：Inno 的 Pascal Script 没有 Delphi 的 StringReplace，只有 StringChangeEx。
function SafeVal(const v: String): String;
var
  s: String;
begin
  s := Trim(v);
  StringChangeEx(s, '"', '', True);
  Result := s;
end;

// GetRoleStr 返回角色字符串。安装阶段读向导选择；卸载阶段回读注册表。
function GetRoleStr(Param: String): String;
var
  r: String;
begin
  r := '';
  if Assigned(PageRole) then
  begin
    if PageRole.Values[0] and PageRole.Values[1] then r := 'all'
    else if PageRole.Values[1] then r := 'server'
    else if PageRole.Values[0] then r := 'agent';
  end;
  if r = '' then
  begin
    if not RegQueryStringValue(HKLM, 'Software\MoCenter', 'Role', r) then
      r := 'all'; // 兜底：引擎对"未安装的角色"会安全跳过
  end;
  Result := r;
end;

// GetDataDirStr 返回数据根目录。安装阶段读向导；卸载阶段向导未创建，
// 只能回读安装时写入注册表的 DataDir（自定义目录也能删对地方）。
function GetDataDirStr(Param: String): String;
var
  v: String;
begin
  if Assigned(PageAdv) then
  begin
    v := SafeVal(PageAdv.Values[0]);
    if v <> '' then
    begin
      Result := v;
      Exit;
    end;
  end;
  if RegQueryStringValue(HKLM, 'Software\MoCenter', 'DataDir', v) then
    Result := v
  else
    Result := ExpandConstant('{commonappdata}\MoCenter');
end;

// HasPurgeSwitch 判断静默卸载是否显式带了 /PURGE。
// Inno 没有现成的 CmdLineParamExists，用 ParamStr 自行比对（大小写不敏感）。
function HasPurgeSwitch: Boolean;
var
  i: Integer;
begin
  Result := False;
  for i := 1 to ParamCount do
    if Lowercase(ParamStr(i)) = '/purge' then
    begin
      Result := True;
      Exit;
    end;
end;

// PasswordPath 口令文件路径。安装引擎读完即删，因此全程只在 ssPostInstall 期间存在。
var
  PasswordPath: String;

// WritePasswordFile 把 Server 口令写入仅管理员可读的临时文件，返回其路径。
//
// 为什么不用命令行传口令：安装向导进程的命令行在安装期间可被同机其他用户读到
// （`wmic process get commandline`、任务管理器"命令行"列），
// 而服务本身的 binPath 我们是不放口令的 —— 若这里用命令行传，等于把前面的防护绕过去了。
//
// 写失败时**不降级**为命令行传参：那正好是我们要避免的那条路。
// 宁可让安装停下并提示原因。
function WritePasswordFile(const Password: String): String;
var
  p: String;
begin
  Result := '';
  if Password = '' then
    Exit;
  p := ExpandConstant('{tmp}\mc-admin-pw');
  if not SaveStringToFile(p, Password, False) then
  begin
    MsgBox('无法写入临时口令文件：' + p + #13#10#13#10 +
           '请不要用 -password 之类的命令行方式绕过（那会让口令对同机其他用户可见）。',
           mbCriticalError, MB_OK);
    Abort;
  end;
  Result := p;
end;

// AgentKeyPath 注册凭据文件路径（与口令文件同一套做法，见 WritePasswordFile）。
var
  AgentKeyPath: String;

// WriteAgentKeyFile 把注册密钥写进临时文件，只把**路径**交给安装引擎（-agent-key-file）。
//
// 为什么单独做一遍而不像以前那样直接拼 -agent-key "<值>"：命令行在安装那几十秒里
// 对同机任意进程可见（wmic process get commandline / 任务管理器"命令行"列），
// 而注册密钥是 C-1 那套一次性令牌的根凭据 —— 泄露面比后台口令更大，
// 偏偏它以前就明晃晃地挂在这条最容易被人路过一眼的通道上。
//
// 写失败时同样**不降级**回命令行传参，宁可停下来说明原因。
function WriteAgentKeyFile(const Key: String): String;
var
  p: String;
begin
  Result := '';
  if Key = '' then
    Exit;
  p := ExpandConstant('{tmp}\mc-agent-key');
  if not SaveStringToFile(p, Key, False) then
  begin
    MsgBox('无法写入临时注册凭据文件：' + p + #13#10#13#10 +
           '请不要改回用 -agent-key 命令行传参（那会让密钥对同机其他用户可见）。',
           mbCriticalError, MB_OK);
    Abort;
  end;
  Result := p;
end;

// GetLogLevelStr 返回向导选择的级别字符串。
function GetLogLevelStr(Param: String): String;
begin
  if PageLog.Values[0] then
    Result := 'off'
  else if PageLog.Values[1] then
    Result := 'error'
  else if PageLog.Values[2] then
    Result := 'warn'
  else
    Result := 'info';
end;

// BuildInstallParams 把向导收集的参数拼成 mocenter-deploy.exe 的命令行。
function BuildInstallParams(Param: String): String;
var
  role, s: String;
begin
  role := GetRoleStr('');
  s := 'install -role ' + role +
       ' -dir "' + ExpandConstant('{app}') + '"' +
       ' -data "' + GetDataDirStr('') + '"';

  if Pos('agent', role) > 0 then
  begin
    s := s + ' -server "' + SafeVal(PageAgent.Values[0]) + '"';
    // 绑定令牌要求 agent_id 与签发时填写的值完全一致；不绑定令牌（签发时留空）
    // 不传名称即可。不传时 Agent 回退为机器 UUID/主机名，身份由该机决定。
    if SafeVal(PageAgent.Values[1]) <> '' then
      s := s + ' -agentid "' + SafeVal(PageAgent.Values[1]) + '"';
    // 注册密钥只走文件，不拼进命令行 —— 引擎读完即删该文件（见 WriteAgentKeyFile）。
    // 命令行在安装那几十秒里对同机任意进程可见，而这是 C-1 一次性令牌的根凭据。
    // 留空时干脆不传：引擎会明确报"必须指定 -agent-key 或 -agent-key-file"，
    // 比偷偷退回命令行传参好（那条路正是这里要避免的）。
    if AgentKeyPath <> '' then
      s := s + ' -agent-key-file "' + AgentKeyPath + '"';
  end;

  if Pos('server', role) > 0 then
  begin
    s := s + ' -user "' + SafeVal(PageServer.Values[0]) + '"';
    // 口令**不拼进命令行**，改由口令文件传递（见 WritePasswordFile）。
    // 命令行在安装进行的那几秒里对其他本地进程可见
    // （wmic process get commandline / 任务管理器"命令行"列），
    // 等于把后台口令广播出去；写成仅管理员可读的文件则没有这个窗口。
    if PasswordPath <> '' then
      s := s + ' -password-file "' + PasswordPath + '"';
    s := s + ' -listen "' + SafeVal(PageAdvServer.Values[0]) + '"';
    s := s + ' -http "' + SafeVal(PageAdvServer.Values[1]) + '"';
    if DirExists(ExpandConstant('{app}\web')) then
      s := s + ' -web "' + ExpandConstant('{app}\web') + '"';
  end;

  // 日志级别只写进配置文件（不塞服务命令行），升级时引擎也只改这一个键。
  s := s + ' -log-level "' + GetLogLevelStr('') + '"';

  if SafeVal(PageAdv.Values[1]) <> '' then
    s := s + ' -proxy-ip "' + SafeVal(PageAdv.Values[1]) + '"';
  if SafeVal(PageAdv.Values[2]) <> '' then
    s := s + ' -extra "' + SafeVal(PageAdv.Values[2]) + '"';

  Result := s;
end;

// ExtractJsonStringValue 从 JSON 文本里取一个字符串字段（不引入 JSON 库，够用即可）。
// Inno 的 Pascal Script 没有正规 JSON 解析，这里只处理本项目自己写出的缩进格式。
// 注意：循环一律写成显式边界判断，Pascal 的 and 不保证短路，
// 写成 (i <= n) and (rest[i] <> ...) 在 i 越界后仍可能去取 rest[i] 而报错。
function ExtractJsonStringValue(const content: String; const key: String): String;
var
  p, i, n: Integer;
  rest: String;
begin
  Result := '';
  p := Pos(key, content);
  if p = 0 then Exit;
  rest := Copy(content, p + Length(key), Length(content));
  n := Length(rest);
  i := 1;
  while i <= n do
  begin
    if rest[i] = ':' then Break;
    Inc(i);
  end;
  if i > n then Exit;
  while i <= n do
  begin
    if rest[i] = '"' then Break;
    Inc(i);
  end;
  if i > n then Exit;
  Inc(i);
  p := i;
  while i <= n do
  begin
    if rest[i] = '"' then Break;
    Inc(i);
  end;
  if i <= n then
    Result := Lowercase(Copy(rest, p, i - p));
end;

// ReadCfgLogLevel 读取某角色既有配置里的 log_level；没有则返回空串。
function ReadCfgLogLevel(const role, dataRoot: String): String;
var
  cfgPath: String;
  // Inno 6 的 LoadStringFromFile 第二个参数是 AnsiString（传 String 会 Type mismatch）；
  // 配置里的 log_level 是纯 ASCII，用 AnsiString 读取没有编码问题。
  cfgText: AnsiString;
begin
  Result := '';
  cfgPath := AddBackslash(dataRoot) + role + '\mocenter-' + role + '.json';
  if not FileExists(cfgPath) then Exit;
  if LoadStringFromFile(cfgPath, cfgText) = False then Exit;
  Result := ExtractJsonStringValue(cfgText, '"log_level"');
end;

// DetectExistingLogLevel 升级时优先沿用 Agent 的级别，其次 Server 的。
function DetectExistingLogLevel(const dataRoot: String): String;
begin
  Result := '';
  if Pos('agent', GetRoleStr('')) > 0 then
    Result := ReadCfgLogLevel('agent', dataRoot);
  if (Result = '') and (Pos('server', GetRoleStr('')) > 0) then
    Result := ReadCfgLogLevel('server', dataRoot);
end;

// SelectLogLevel 把级别字符串映射到单选索引；空/未知时落到 ERROR。
procedure SelectLogLevel(const lv: String);
begin
  if lv = 'off' then
    PageLog.Values[0] := True
  else if lv = 'warn' then
    PageLog.Values[2] := True
  else if lv = 'info' then
    PageLog.Values[3] := True
  else
    PageLog.Values[1] := True;
end;

procedure InitializeWizard;
var
  prevDataDir: String;
begin
  // 第 1 页：角色
  PageRole := CreateInputOptionPage(wpWelcome,
    '选择安装角色',
    '本机要安装哪些组件',
    'Agent 部署在被管理的主机上；Server 是集中管理平台（通常只装一台）。',
    True, False);
  PageRole.Add('Agent —— 被控端（部署到每台被管理的主机）');
  PageRole.Add('Server —— 总控端（集中管理平台）');
  PageRole.Values[0] := True;

  // 第 2 页：Agent 专用（未选 Agent 时由 ShouldSkipPage 跳过）
  PageAgent := CreateInputQueryPage(PageRole.ID,
    'Agent 连接与认证',
    '填写被控端（Agent）参数',
    '本页只包含 Agent 需要的参数；Server 的账号与监听端口不会出现在这里。' + #13#10 +
    'Agent 名称/ID：绑定令牌必须与总控签发时填写的 agent_id 完全一致；不绑定令牌可留空。' + #13#10 +
    '不知道本机 UUID？在目标机 PowerShell 执行 (Get-CimInstance Win32_ComputerSystemProduct).UUID，' +
    '签发绑定令牌时填同一个值；本页留空则 Agent 会用该 UUID。' + #13#10 +
    '升级时三项都可以留空，沿用既有配置。');
  PageAgent.Add('服务端地址（host:port，必填）：', False);
  // Agent 名称/ID：绑定令牌按 agent_id 校验，必须与总控签发时填写的值完全一致；
  // 不绑定令牌（签发时留空）没有这个约束。留空时 Agent 回退成机器 UUID/主机名。
  PageAgent.Add('Agent 名称/ID（绑定令牌须与签发时一致；留空则使用机器 UUID）：', False);
  // L-9：注册密钥是**等价于主机身份**的凭据（拿到它就能为任意 agent_id 换证书），
  // 输入框最后一位参数 True 表示掩码回显 —— 旧版为 False，等于在安装向导界面上
  // 明文显示它，任何能瞄到屏幕的人/录屏/截图都能抄走。
  // 注意 LastChar 仍会被读进 Process2，属 Inno 的固有限制；掩码至少挡住旁人旁观。
  PageAgent.Add('Agent 注册密钥或一次性令牌（必填；新主机请粘贴总控 /admin/enroll ② 签发的令牌）：', True);

  // 第 3 页：Server 专用（未选 Server 时跳过）
  PageServer := CreateInputQueryPage(PageAgent.ID,
    'Server 后台认证',
    '填写总控端（Server）参数',
    '本页只包含 Server 需要的参数；Agent 的服务端地址与注册令牌不会出现在这里。');
  PageServer.Add('后台登录用户名：', False);
  PageServer.Add('后台登录密码（至少 12 位）：', True);
  PageServer.Values[0] := 'admin';

  // 第 4 页：公共高级项
  PageAdv := CreateInputQueryPage(PageServer.ID,
    '高级设置',
    '目录与网络（Agent / Server 通用）',
    '以下均有默认值，不确定时直接点“下一步”即可。');
  PageAdv.Add('数据目录（配置/数据库/日志）：', False);
  PageAdv.Add('国内加速代理前缀（可留空）：', False);
  PageAdv.Add('TLS 参数（可留空，如 -tls -ca-cert C:\ca.crt）：', False);

  // 第 5 页：Server 监听（未选 Server 时跳过）
  PageAdvServer := CreateInputQueryPage(PageAdv.ID,
    'Server 监听设置',
    '仅 Server 使用的监听地址',
    '以下仅对 Server 生效，安装 Agent 时不会使用这些值。');
  PageAdvServer.Add('Server 的 Agent 监听地址：', False);
  PageAdvServer.Add('Server 的 HTTP 监听地址：', False);

  // 第 6 页：日志级别（Agent/Server 通用；升级时预选既有配置里的值）
  PageLog := CreateInputOptionPage(PageAdvServer.ID,
    '运行期日志级别',
    '日志写到什么详细程度',
    'Agent 默认 error（只记失败），Server 默认 info（日志量小，启动信息更利于排障）。' + #13#10 +
    'OFF 关闭运行期日志，但启动致命错误仍会输出一行（否则服务起不来时零线索）。' + #13#10 +
    '装好后可在控制台「仪表盘 → 操作 → 日志级别」随时远程调整。',
    True, False);
  PageLog.Add('OFF —— 关闭运行期日志');
  PageLog.Add('ERROR —— 只记录失败（推荐，默认）');
  PageLog.Add('WARN —— 失败 + 重试/降级');
  PageLog.Add('INFO —— 完整生命周期（排障用，日志量最大）');

  // 重装/升级沿用上次记录的数据目录，避免自定义目录被默认值顶掉；
  // 全新安装时注册表没有该值，仍用 ProgramData 默认路径。
  if RegQueryStringValue(HKLM, 'Software\MoCenter', 'DataDir', prevDataDir) then
    PageAdv.Values[0] := prevDataDir
  else
    PageAdv.Values[0] := ExpandConstant('{commonappdata}\MoCenter');
  PageAdv.Values[1] := '';
  PageAdv.Values[2] := '';
  PageAdvServer.Values[0] := ':9000';
  PageAdvServer.Values[1] := ':8080';
  // 升级：预选既有配置里的级别，避免把运维调好的 info 悄悄改回 error。
  SelectLogLevel(DetectExistingLogLevel(PageAdv.Values[0]));
end;

// ShouldSkipPage 按角色选择跳过专有页：
//   只装 Agent → 不出现 Server 认证页与监听页；
//   只装 Server → 不出现 Agent 连接页；
//   两者都选   → 全部显示。
function ShouldSkipPage(PageID: Integer): Boolean;
var
  role: String;
begin
  Result := False;
  if (PageID <> PageAgent.ID) and (PageID <> PageServer.ID) and (PageID <> PageAdvServer.ID) then
    Exit;
  role := GetRoleStr('');
  if PageID = PageAgent.ID then
    Result := Pos('agent', role) = 0
  else
    Result := Pos('server', role) = 0;
end;

// NextButtonClick 在离开"必要项"页时校验必填项，避免装完才发现参数缺失。
// InstalledRoleHasConfig 判断某角色是否已装过（配置存在）。
// 已装过时向导不再强制填写连接参数 —— 引擎会按「升级」处理并保留原配置，
// 硬性拦截只会让运维在升级时卡在"必须重新输入服务端地址"这一页。
function InstalledRoleHasConfig(role: String): Boolean;
var
  base, sub: String;
begin
  if Pos('server', role) > 0 then
    sub := '\server\mocenter-server.json'
  else
    sub := '\agent\mocenter-agent.json';
  base := SafeVal(PageAdv.Values[0]);
  Result := FileExists(base + sub);
  // 旧版本安装可能用了自定义数据目录（当时未记录）：回读注册表再判一次，
  // 避免升级时误以为"没装过"而强迫重新填写连接参数。
  if (not Result) and RegQueryStringValue(HKLM, 'Software\MoCenter', 'DataDir', base) then
    Result := FileExists(base + sub);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;

  if CurPageID = PageRole.ID then
  begin
    if (not PageRole.Values[0]) and (not PageRole.Values[1]) then
    begin
      MsgBox('请至少选择一个安装角色（Agent 或 Server）。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
  end;

  if CurPageID = PageAgent.ID then
  begin
    if InstalledRoleHasConfig('agent') then
      Exit; // 升级路径：配置与注册凭据都沿用既有的
    if SafeVal(PageAgent.Values[0]) = '' then
    begin
      MsgBox('安装 Agent 必须填写服务端地址（host:port）。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if SafeVal(PageAgent.Values[2]) = '' then
    begin
      MsgBox('安装 Agent 必须填写注册密钥或一次性令牌。' + #13#10#13#10 +
             '新主机请在总控的「Agent 接入」页 ② 签发一枚一次性令牌并粘贴到这里；' +
             '旧机器仍可用总控数据目录下的 agent.key。没有它，Agent 的连接会被服务端拒绝。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
  end;

  if CurPageID = PageServer.ID then
  begin
    if InstalledRoleHasConfig('server') then
      Exit; // 升级路径：账号沿既有配置
    if SafeVal(PageServer.Values[0]) = '' then
    begin
      MsgBox('安装 Server 必须填写后台登录用户名。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    // L-9：下限由 6 提到 12。服务端的 PBKDF2 会把离线爆破成本抬得很高，但
    // 6 位口令在**在线**尝试下也只需几小时（且这是后台唯一的口令闸门），
    // 顺带与 README/docs 里"强口令"的口径对齐。
    if Length(SafeVal(PageServer.Values[1])) < 12 then
    begin
      MsgBox('安装 Server 必须设置至少 12 位的后台登录密码。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
  end;
end;

// CurStepChanged 在文件复制完成后调用安装引擎注册服务。
// 放在这里而不是 [Run]，是为了能拿到退出码并在失败时中止安装 ——
// 否则"文件装好了但服务没起来"，用户看到的却是成功。
procedure CurStepChanged(CurStep: TSetupStep);
var
  rc: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    WizardForm.StatusLabel.Caption := '正在注册系统服务并启动...';
    // 口令与注册密钥都先落文件（只在本次安装期间存在；安装引擎读完会立即删除）。
    // 拼在命令行上会让它们对同机任意进程可见 —— 任务管理器"命令行"列就能看到。
    if Pos('server', GetRoleStr('')) > 0 then
      PasswordPath := WritePasswordFile(SafeVal(PageServer.Values[1]));
    if Pos('agent', GetRoleStr('')) > 0 then
      AgentKeyPath := WriteAgentKeyFile(SafeVal(PageAgent.Values[2]));
    if not Exec(ExpandConstant('{app}\mocenter-deploy.exe'), BuildInstallParams(''),
                ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc) then
    begin
      MsgBox('无法运行安装引擎 mocenter-deploy.exe。', mbCriticalError, MB_OK);
      Abort;
    end;
    if rc <> 0 then
    begin
      MsgBox('服务注册失败（退出码 ' + IntToStr(rc) + '）。' + #13#10#13#10 +
             '详细原因已记录在：' + #13#10 +
             '  ' + ExpandConstant('{app}') + '\mocenter-deploy.log' + #13#10#13#10 +
             '服务运行期的日志位于：' + #13#10 +
             '  ' + GetDataDirStr('') + '\<角色>\logs',
             mbCriticalError, MB_OK);
      Abort;
    end;
  end;
end;

// CurUninstallStepChanged 在卸载真正开始前询问：是否连同配置与数据一起删除。
// 默认「否」保留；静默卸载不弹窗，只有显式传 /PURGE 才清理。
//
// 服务移除与数据清理在这里直接调 mocenter-deploy.exe，而不是走 [UninstallRun]：
// [UninstallRun] 的参数会在**安装阶段**求值并写进卸载日志，而这里的 PurgeData
// 要到卸载时才由用户决定，放进 [UninstallRun] 永远是旧值。
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Answer, rc: Integer;
  params: String;
begin
  if CurUninstallStep = usUninstall then
  begin
    PurgeData := False;
    if UninstallSilent then
      PurgeData := HasPurgeSwitch
    else
    begin
      Answer := MsgBox('是否同时删除 MoCenter 的配置与数据？' + #13#10#13#10 +
        '将删除：' + GetDataDirStr('') + '\<角色>\ 下的配置、数据库、证书与日志。' + #13#10#13#10 +
        '选择「否」将保留这些文件（默认），方便日后重装时沿用。',
        mbConfirmation, MB_YESNO or MB_DEFBUTTON2);
      PurgeData := Answer = IDYES;
    end;

    params := 'uninstall -role ' + GetRoleStr('') +
              ' -dir "' + ExpandConstant('{app}') + '"' +
              ' -data "' + GetDataDirStr('') + '"' +
              ' -keep-binaries';
    if PurgeData then
      params := params + ' -purge';

    // -keep-binaries：二进制由 Inno 统一删除，避免两边抢着删。
    if not Exec(ExpandConstant('{app}\mocenter-deploy.exe'), params,
                ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, rc) then
      MsgBox('无法运行卸载引擎，服务可能仍在运行。' + #13#10#13#10 +
             '请手动执行：' + #13#10 +
             ExpandConstant('{app}') + '\mocenter-deploy.exe ' + params,
             mbError, MB_OK)
    else if rc <> 0 then
      MsgBox('卸载引擎返回错误（退出码 ' + IntToStr(rc) + '），服务或数据可能未完全清理。' + #13#10#13#10 +
             '详情见：' + ExpandConstant('{app}') + '\mocenter-deploy.log',
             mbError, MB_OK);
  end;
end;
