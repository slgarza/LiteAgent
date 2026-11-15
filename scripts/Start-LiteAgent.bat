@echo off
setlocal
cd /d "%~dp0"
start "" "%~dp0LiteAgent.exe" serve --data "%~dp0data" %*
