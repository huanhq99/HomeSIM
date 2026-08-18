param([switch]$NoLaunch)

$ErrorActionPreference = "Stop"
$SourceDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\MacCellular"
$StartMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
$Executable = Join-Path $InstallDir "MacCellular.exe"

Write-Host "Installing MacCellular to $InstallDir"
Get-Process -Name "MacCellular" -ErrorAction SilentlyContinue | Stop-Process -Force

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force (Join-Path $SourceDir "MacCellular.exe") $Executable
Copy-Item -Force (Join-Path $SourceDir "uninstall.ps1") (Join-Path $InstallDir "uninstall.ps1")
Copy-Item -Force (Join-Path $SourceDir "README-Windows.txt") (Join-Path $InstallDir "README-Windows.txt")
Copy-Item -Force (Join-Path $SourceDir "LICENSE") (Join-Path $InstallDir "LICENSE")
Copy-Item -Force (Join-Path $SourceDir "VERSION") (Join-Path $InstallDir "VERSION")
Copy-Item -Force (Join-Path $SourceDir "THIRD_PARTY_NOTICES.md") (Join-Path $InstallDir "THIRD_PARTY_NOTICES.md")
foreach ($LicenseTree in @("licenses", "third_party")) {
    $SourceTree = Join-Path $SourceDir $LicenseTree
    $DestinationTree = Join-Path $InstallDir $LicenseTree
    New-Item -ItemType Directory -Force -Path $DestinationTree | Out-Null
    Copy-Item -Recurse -Force (Join-Path $SourceTree "*") $DestinationTree
}

$Shell = New-Object -ComObject WScript.Shell
$Shortcut = $Shell.CreateShortcut((Join-Path $StartMenu "MacCellular.lnk"))
$Shortcut.TargetPath = $Executable
$Shortcut.WorkingDirectory = $InstallDir
$Shortcut.Description = "MacCellular self-hosted cellular calls and SMS"
$Shortcut.Save()

$UninstallShortcut = $Shell.CreateShortcut((Join-Path $StartMenu "Uninstall MacCellular.lnk"))
$UninstallShortcut.TargetPath = "powershell.exe"
$UninstallShortcut.Arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$(Join-Path $InstallDir 'uninstall.ps1')`""
$UninstallShortcut.WorkingDirectory = $InstallDir
$UninstallShortcut.Save()

Write-Host "MacCellular installation completed."
if (-not $NoLaunch) {
    Start-Process $Executable
}
