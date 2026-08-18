@echo off
taskkill /IM MacCellular.exe /T /F >nul 2>&1
if errorlevel 1 (
  echo MacCellular is not running.
) else (
  echo MacCellular stopped.
)
timeout /t 2 >nul
