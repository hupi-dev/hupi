# Installs VS Code (Microsoft's system installer) in the test VM. Runs once,
# from install.bat, at the end of the unattended Windows install. Z: is the
# host's shared folder.
$ErrorActionPreference = 'Continue'
# Without this, Windows PowerShell 5.1's progress bar slows Invoke-WebRequest
# to a crawl (the ~250 MB installer took half an hour).
$ProgressPreference = 'SilentlyContinue'
"setup start: $(Get-Date) as $env:USERNAME" | Out-File Z:\setup-status.txt
$installer = "$env:TEMP\vscode-setup.exe"
Invoke-WebRequest -UseBasicParsing 'https://update.code.visualstudio.com/latest/win32-x64/stable' -OutFile $installer
Start-Process -Wait $installer -ArgumentList '/VERYSILENT', '/NORESTART', '/MERGETASKS=!runcode'
"vscode installed: $(Test-Path 'C:\Program Files\Microsoft VS Code\Code.exe')" | Out-File -Append Z:\setup-status.txt
"setup done: $(Get-Date)" | Out-File -Append Z:\setup-status.txt
