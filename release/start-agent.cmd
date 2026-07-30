@echo off
setlocal

set "CONFIG_FILE=%REMOTE_SYNC_CONFIG%"
if not defined CONFIG_FILE set "CONFIG_FILE=%~dp0remote-sync.env"

if not exist "%CONFIG_FILE%" (
  echo Remote Sync configuration was not found:
  echo   %CONFIG_FILE%
  echo.
  echo Copy remote-sync.env.example to remote-sync.env, edit the connection values, and start again.
  exit /b 2
)

for /f "usebackq eol=# tokens=1,* delims==" %%A in ("%CONFIG_FILE%") do (
  if not "%%A"=="" set "%%A=%%B"
)

"%~dp0sync-agent.exe" %*
exit /b %ERRORLEVEL%
