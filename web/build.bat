@echo off
REM MoCenter Web 前端构建脚本 (Windows)
cd /d "%~dp0"
echo [web] npm install ...
call npm install --no-audit --no-fund
echo [web] vite build ...
call npm run build
echo [web] done -^> .\dist
