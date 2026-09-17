; =============================================================
; MoCenter Windows 图形安装向导（Inno Setup 6）
;
; 构建前置：
;   1) 运行 scripts\build-all.bat --type=installer
;      —— 它会编译 agent / server / 安装引擎到 installer\payload\，再调用本脚本
;   2) 编译本脚本：
;        "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" installer\setup.iss
;      产物：build\MoCenter-Setup-<版本>.exe
;
; 设计说明：
;   - 向导只负责「收集参数 + 摆放文件」，真正的服务注册、配置落盘、
;     单实例与日志重定向仍由 mocenter-deploy.exe 完成，避免两套逻辑各写一遍。
;   - 因此本脚本里没有任何服务注册代码，只有界面与参数拼装。
;
; 关于中文界面：
;   向导自定义页面（标题、说明、字段）全部是中文；框架按钮（下一步/安装等）
;   的文案覆盖放在 messages-cn.iss，若 ISCC 报语言前缀错误，只需改那一个文件。
; =============================================================

#define AppName "MoCenter"
; 版本号在本文件与 Go 源码 internal/version 中**必须一致**。
; 发布纪律：只在明确发版时修改，且 server / agent 共用同一版本号。
; 一致性由 server/internal/version 的单元测试守护（含对 agent 源码的比对）。
#define AppVersion "1.0.1"
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
OutputDir=..\build
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

[UninstallRun]
; 先移除服务，再由 Inno 删除文件（-keep-binaries 让引擎不要抢着删）
Filename: "{app}\mocenter-deploy.exe"; \
  Parameters: "uninstall -role {code:GetRoleStr} -dir ""{app}"" -keep-binaries"; \
  Flags: runhidden waituntilterminated; RunOnceId: "RemoveMoCenterServices"

[Code]
var
  PageRole: TInputOptionWizardPage;
  PageBasic: TInputQueryWizardPage;
  PageAdv: TInputQueryWizardPage;

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

// BuildInstallParams 把向导收集的参数拼成 mocenter-deploy.exe 的命令行。
function BuildInstallParams(Param: String): String;
var
  role, s: String;
begin
  role := GetRoleStr('');
  s := 'install -role ' + role +
       ' -dir "' + ExpandConstant('{app}') + '"' +
       ' -data "' + SafeVal(PageAdv.Values[0]) + '"';

  if Pos('agent', role) > 0 then
  begin
    s := s + ' -server "' + SafeVal(PageBasic.Values[0]) + '"';
    s := s + ' -agent-key "' + SafeVal(PageBasic.Values[3]) + '"';
  end;

  if Pos('server', role) > 0 then
  begin
    s := s + ' -user "' + SafeVal(PageBasic.Values[1]) + '"';
    s := s + ' -password "' + SafeVal(PageBasic.Values[2]) + '"';
    s := s + ' -listen "' + SafeVal(PageAdv.Values[1]) + '"';
    s := s + ' -http "' + SafeVal(PageAdv.Values[2]) + '"';
    if DirExists(ExpandConstant('{app}\web')) then
      s := s + ' -web "' + ExpandConstant('{app}\web') + '"';
  end;

  if SafeVal(PageAdv.Values[3]) <> '' then
    s := s + ' -proxy-ip "' + SafeVal(PageAdv.Values[3]) + '"';
  if SafeVal(PageAdv.Values[4]) <> '' then
    s := s + ' -extra "' + SafeVal(PageAdv.Values[4]) + '"';

  Result := s;
end;

procedure InitializeWizard;
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

  // 第 2 页：必要项
  PageBasic := CreateInputQueryPage(PageRole.ID,
    '连接与认证',
    '填写所选角色必需的参数',
    '只需填当前角色需要的项，另一项留空不影响安装。');
  PageBasic.Add('服务端地址（host:port，安装 Agent 时必填）：', False);
  PageBasic.Add('Server 后台登录用户名（安装 Server 时必填）：', False);
  PageBasic.Add('Server 后台登录密码（安装 Server 时必填，至少 6 位）：', True);
  PageBasic.Add('Agent 注册密钥（安装 Agent 时必填，取总控机数据目录下 agent.key 的内容）：', False);

  // 第 3 页：高级
  PageAdv := CreateInputQueryPage(PageBasic.ID,
    '高级设置',
    '端口、目录与网络',
    '以下均有默认值，不确定时直接点“下一步”即可。');
  PageAdv.Add('数据目录（配置/数据库/日志）：', False);
  PageAdv.Add('Server 的 Agent 监听地址：', False);
  PageAdv.Add('Server 的 HTTP 监听地址：', False);
  PageAdv.Add('国内加速代理前缀（可留空）：', False);
  PageAdv.Add('TLS 参数（可留空，如 -tls -ca-cert C:\ca.crt）：', False);

  PageAdv.Values[0] := ExpandConstant('{commonappdata}\MoCenter');
  PageAdv.Values[1] := ':9000';
  PageAdv.Values[2] := ':8080';
  PageAdv.Values[3] := '';
  PageAdv.Values[4] := '';
  PageBasic.Values[1] := 'admin';
end;

// NextButtonClick 在离开"必要项"页时校验必填项，避免装完才发现参数缺失。
function NextButtonClick(CurPageID: Integer): Boolean;
var
  role: String;
begin
  Result := True;
  if CurPageID = PageBasic.ID then
  begin
    role := GetRoleStr('');
    if (Pos('agent', role) > 0) and (SafeVal(PageBasic.Values[0]) = '') then
    begin
      MsgBox('安装 Agent 必须填写服务端地址（host:port）。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if (Pos('agent', role) > 0) and (SafeVal(PageBasic.Values[3]) = '') then
    begin
      MsgBox('安装 Agent 必须填写 Agent 注册密钥。' + #13#10#13#10 +
             '该密钥在总控机的数据目录下（agent.key），服务端用它在应用层校验 Agent 身份；' +
             '没有它，Agent 的连接会被服务端拒绝。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if (Pos('server', role) > 0) and (SafeVal(PageBasic.Values[1]) = '') then
    begin
      MsgBox('安装 Server 必须填写后台登录用户名。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if (Pos('server', role) > 0) and (Length(SafeVal(PageBasic.Values[2])) < 6) then
    begin
      MsgBox('安装 Server 必须设置至少 6 位的后台登录密码。', mbError, MB_OK);
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
             '  ' + SafeVal(PageAdv.Values[0]) + '\<角色>\logs',
             mbCriticalError, MB_OK);
      Abort;
    end;
  end;
end;
