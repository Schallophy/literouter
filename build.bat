@echo off
setlocal
cd /d "%~dp0"

where go >nul 2>nul
if errorlevel 1 (
  echo [build] Go not found in PATH.
  exit /b 1
)

set "CGO_ENABLED=0"
echo [build] Building literouter.exe ...
go build -trimpath -ldflags "-s -w" -o literouter.exe .
if errorlevel 1 (
  echo [build] Build failed.
  exit /b 1
)

echo [build] Done: %CD%\literouter.exe
exit /b 0
