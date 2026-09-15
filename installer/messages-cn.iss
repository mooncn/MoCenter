; =============================================================
; 向导「框架文案」的中文覆盖（被 setup.iss 通过 #include 引入）
;
; 为什么这么做：Inno 官方安装包默认不含简体中文语言文件，而每个语言文件必须完整，
; 自己造一份不现实。所以这里直接覆盖默认（English）语言的关键文案 ——
; 自定义页面的文字本来就在 setup.iss 里写死为中文，这里补的是框架自带的按钮与提示。
;
; ⚠️ 如果 ISCC 报错，通常是这两类，改法都很简单：
;   1) 语言前缀不对（提示 unknown language 之类）
;      → 把下面所有 english. 改成 en.
;   2) 某个消息名在当前 Inno 版本里不存在（提示 unknown message name）
;      → 删掉那一行即可，其余不受影响
;
; 另一种做法（更彻底）：下载社区翻译 ChineseSimplified.isl 放进 Inno 安装目录的
; Languages 文件夹，然后在 setup.iss 的 [Languages] 里加一行
;     Name: "cn"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"
; 并删除本文件。
; =============================================================

[Messages]
english.ButtonBack=< 上一步(&B)
english.ButtonNext=下一步(&N) >
english.ButtonInstall=安装(&I)
english.ButtonCancel=取消
english.ButtonFinish=完成(&F)

english.WelcomeLabel1=欢迎使用 MoCenter 安装向导
english.WelcomeLabel2=本向导将引导你完成 MoCenter 的安装与系统服务注册。%n%n继续前建议关闭其他应用程序。

english.SelectDirLabel3=将把 MoCenter 安装到以下文件夹。
english.ReadyLabel1=准备安装
; Inno Setup 6.7 起 ReadyLabel2 被拆成 2a / 2b：
;   2a = 可按「上一步」返回检查设置时用；2b = 不可返回时用。
; 只写旧的 ReadyLabel2 会导致 ISCC 报 unknown message name 并被忽略（那一行退回英文）。
english.ReadyLabel2a=安装程序现在已准备好，点击“安装”开始；如需检查或修改设置，请点击“上一步”。
english.ReadyLabel2b=安装程序现在已准备好，点击“安装”开始。

english.FinishedHeadingLabel=MoCenter 安装完成
english.FinishedLabel=MoCenter 已安装完成，系统服务已注册为开机自启。%n%n可运行以下命令查看服务状态：%n  mocenter-deploy.exe status -role all

english.ConfirmUninstall=确定要卸载 MoCenter 吗？%n%n服务会被停止并移除；配置与数据默认保留。
english.UninstalledAll=MoCenter 已从你的计算机中卸载。
