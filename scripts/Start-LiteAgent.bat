@echo off
setlocal
cd /d "%~dp0"
LiteAgent.exe serve --data "%~dp0data" %*
if errorlevel 1 pause
