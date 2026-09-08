@echo off
setlocal
title Kanz Testnet Vault Bootstrap

echo Choose an operation:
echo   1. Rotate exchange testnet credentials
echo   2. Re-run the venue Vault bootstrap
echo   3. Bootstrap generated data-plane credentials and signing keys
set /p "CHOICE=Enter 1, 2, or 3: "

if "%CHOICE%"=="1" set "MODE=Rotate"& set "TARGET=Venue"
if "%CHOICE%"=="2" set "MODE=Bootstrap"& set "TARGET=Venue"
if "%CHOICE%"=="3" set "MODE=Bootstrap"& set "TARGET=DataPlane"
if not defined MODE (
  echo Invalid choice. No changes were made.
  pause
  exit /b 2
)

set "PSHOST="
where pwsh.exe >nul 2>&1
if not errorlevel 1 set "PSHOST=pwsh.exe"
if not defined PSHOST if exist "%USERPROFILE%\.cache\codex-runtimes\codex-primary-runtime\dependencies\native\powershell\pwsh.exe" set "PSHOST=%USERPROFILE%\.cache\codex-runtimes\codex-primary-runtime\dependencies\native\powershell\pwsh.exe"
if not defined PSHOST if exist "%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" set "PSHOST=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
if not defined PSHOST (
  echo PowerShell 7 or Windows PowerShell could not be found. No changes were made.
  pause
  exit /b 3
)

"%PSHOST%" -NoProfile -File "%~dp0Invoke-VenueSecrets.ps1" -Mode %MODE% -Target %TARGET%
if errorlevel 1 (
  echo The operation was not verified. Review the error above.
) else (
  echo The operation completed and was verified.
)
pause
endlocal
