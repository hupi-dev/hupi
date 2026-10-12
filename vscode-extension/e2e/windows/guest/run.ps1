# HUPI agent end-to-end test in the Windows VM, for one app: stock VS Code
# with the Marketplace extension, or HUPI Code with its bundled one. See
# ../README.md. Run inside the VM (host/run-suite.sh types this for you):
#   powershell -ExecutionPolicy Bypass -File Z:\e2e\run.ps1 -App vscode|hupicode [-Mode main|scenario2]
# Results go to Z:\e2e\out-<app>-<mode>\ (result.json, screenshots, logs, DONE).
param(
  [Parameter(Mandatory)][ValidateSet('vscode', 'hupicode')][string]$App,
  [ValidateSet('main', 'scenario2')][string]$Mode = 'main',
  # HUPI's gateway on the host, as the VM sees it (Docker's default bridge).
  [string]$Gateway = 'http://172.17.0.1:18790'
)
$ErrorActionPreference = 'Continue'
$E2E = 'Z:\e2e'
$out = "$E2E\out-$App-$Mode"
Remove-Item -Recurse -Force $out -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $out | Out-Null
function Log($m) { "$(Get-Date -Format HH:mm:ss) $m" | Out-File -Append -Encoding utf8 "$out\run.log" }
Start-Transcript -Path "$out\transcript.txt" | Out-Null

if ($App -eq 'vscode') {
  $exe = 'C:\Program Files\Microsoft VS Code\Code.exe'
  $cli = 'C:\Program Files\Microsoft VS Code\bin\code.cmd'
} else {
  $appDir = 'C:\hupi-e2e\hupi-code-app'
  # The CI artifact (hupi-code-win32-x64), copied off the shared folder.
  Get-Process | Where-Object { $_.Path -and $_.Path -like "$appDir\*" } | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep 2
  Remove-Item -Recurse -Force $appDir -ErrorAction SilentlyContinue
  Copy-Item -Recurse 'Z:\hupicode-artifact' $appDir
  $exe = (Get-ChildItem $appDir -Filter *.exe | Where-Object { $_.Name -notmatch 'unins|crash' } | Select-Object -First 1).FullName
  $cli = (Get-ChildItem "$appDir\bin" -Filter *.cmd | Select-Object -First 1).FullName
}
Log "app=$App exe=$exe cli=$cli"

# Close any running instance of either app.
Get-Process | Where-Object { $_.Path -and ($_.Path -like 'C:\Program Files\Microsoft VS Code\*' -or $_.Path -like 'C:\hupi-e2e\hupi-code-app\*') } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep 3

# A fresh profile and project.
$root = "C:\hupi-e2e\$App"
Remove-Item -Recurse -Force $root -ErrorAction SilentlyContinue
$ud = "$root\user-data"; $ext = "$root\extensions"
New-Item -ItemType Directory -Force -Path "$ud\User", $ext | Out-Null
$proj = "C:\Users\Public\hupi e2e's $App"
Remove-Item -Recurse -Force $proj -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$proj\src" | Out-Null
Set-Content -NoNewline -Path "$proj\src\config.ts" -Value "export const greeting = `"hello`";`nexport const answer = 41;`n"
Copy-Item "$E2E\smoke-mcp.js" "$root\smoke-mcp.js"

$settings = [ordered]@{
  'hupi.baseUrl' = $Gateway
  'hupi.fileContext.enabled' = $false
  'security.workspace.trust.enabled' = $false
  'window.dialogStyle' = 'custom'
  'workbench.startupEditor' = 'none'
  'update.mode' = 'none'
  'extensions.autoCheckUpdates' = $false
  'extensions.autoUpdate' = $false
  'telemetry.telemetryLevel' = 'off'
}
# Stock VS Code: the agent is off by default, so turn it on. HUPI Code: leave
# it unset, to check that it's on by default there.
if ($App -eq 'vscode') { $settings['hupi.agent.enabled'] = $true }
$settings | ConvertTo-Json | Set-Content -Encoding utf8 "$ud\User\settings.json"
$mcp = @{ servers = @{ smoke = @{ type = 'stdio'; command = $exe; args = @("$root\smoke-mcp.js"); env = @{ ELECTRON_RUN_AS_NODE = '1' } } } }
$mcp | ConvertTo-Json -Depth 5 | Set-Content -Encoding utf8 "$ud\User\mcp.json"

# Extensions: the HUPI extension from the Marketplace (stock VS Code only;
# HUPI Code bundles it) and the helper that opens HUPI's sidebar.
if ($App -eq 'vscode') {
  & $cli --user-data-dir $ud --extensions-dir $ext --install-extension hupi.hupi-vscode --force *>> "$out\install.txt"
}
& $cli --user-data-dir $ud --extensions-dir $ext --install-extension "$E2E\opener.vsix" --force *>> "$out\install.txt"
& $cli --user-data-dir $ud --extensions-dir $ext --list-extensions --show-versions *>> "$out\extensions.txt"
Log "extensions: $((Get-Content "$out\extensions.txt") -join ', ')"

Log 'launching'
$p = Start-Process -PassThru -FilePath $exe -ArgumentList @('--user-data-dir', "`"$ud`"", '--extensions-dir', "`"$ext`"", '--remote-debugging-port=9333', '--disable-workspace-trust', "`"$proj`"")
Start-Sleep 5
# Code.exe is a GUI-subsystem program, so PowerShell's & wouldn't wait for it.
$env:ELECTRON_RUN_AS_NODE = '1'
$prompt = 'Read src/config.ts, change answer to 42, check it with a command, then try the smoke MCP tools.'
if ($Mode -eq 'scenario2') {
  Set-Content -Path "$proj\.env" -Value 'SECRET_TOKEN=do-not-read'
  $prompt = "SCENARIO2 ABS=$proj\src\config.ts"
}
$d = Start-Process -PassThru -Wait -NoNewWindow -FilePath $exe -ArgumentList @("`"$E2E\driver.js`"", '9333', "`"$out`"", "`"$($prompt -replace '"', '\"')`"", $Mode) -RedirectStandardError "$out\driver-stderr.txt"
Remove-Item Env:\ELECTRON_RUN_AS_NODE
Log "driver exit $($d.ExitCode)"
Log 'driver finished'

Get-Content -Raw "$proj\src\config.ts" | Out-File -Encoding utf8 "$out\config-after.txt"
Get-ChildItem -Recurse "$ud\logs" -Filter *.log -ErrorAction SilentlyContinue | Where-Object { $_.FullName -match 'exthost|mcp|HUPI|hupi' } | ForEach-Object { Copy-Item $_.FullName ("$out\log-" + ($_.FullName.Substring($ud.Length + 6) -replace '[\\/]', '_')) }
Start-Sleep 2
Get-Process | Where-Object { $_.Path -and $_.Path -eq $exe } | Stop-Process -Force -ErrorAction SilentlyContinue
Log 'done'
Stop-Transcript | Out-Null
'done' | Out-File "$out\DONE"
