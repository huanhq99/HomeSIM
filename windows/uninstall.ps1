$ErrorActionPreference = "Stop"
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\MacCellular"
$StartMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"

Get-Process -Name "MacCellular" -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $StartMenu "MacCellular.lnk")
Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $StartMenu "Uninstall MacCellular.lnk")

if (Test-Path $InstallDir) {
    Remove-Item -Recurse -Force $InstallDir
}

Write-Host "MacCellular has been uninstalled."
