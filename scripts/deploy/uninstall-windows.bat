@echo off
REM MoCenter Windows 服务卸载（需管理员）—— 等价于 install-windows.bat uninstall，
REM 但控制面板式的「卸载」入口更容易被理解，故单独提供一份。
call "%~dp0install-windows.bat" uninstall
