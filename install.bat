@echo off
setlocal
cd /d "%~dp0"

where go >nul 2>nul
if errorlevel 1 (
  echo [install] Go not found in PATH.
  exit /b 1
)

set "CGO_ENABLED=0"
echo [install] Installing with go install ...
go install -trimpath -ldflags "-s -w" .
if errorlevel 1 (
  echo [install] Install failed.
  exit /b 1
)

for /f "delims=" %%i in ('go env GOBIN') do set "GOBIN=%%i"
if not defined GOBIN (
  for /f "delims=" %%i in ('go env GOPATH') do set "GOPATH=%%i"
  set "GOBIN=%GOPATH%\bin"
)

if not exist "%GOBIN%\literouter.exe" (
  echo [install] Binary not found at %GOBIN%\literouter.exe
  exit /b 1
)

echo [install] Done: %GOBIN%\literouter.exe
echo [install] If "literouter" is not recognized, add %GOBIN% to PATH.
exit /b 0
