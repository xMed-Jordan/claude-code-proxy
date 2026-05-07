$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$startScript = Join-Path $basePath 'start-proxy.ps1'
$taskName = 'ClaudeCodeCodexProxy'

if (-not (Test-Path $startScript)) {
    throw "Missing startup script: $startScript"
}

$action = New-ScheduledTaskAction `
    -Execute 'pwsh.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$startScript`"" `
    -WorkingDirectory $basePath

$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 0) `
    -MultipleInstances IgnoreNew

Register-ScheduledTask `
    -TaskName $taskName `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -Description 'Starts the local Claude Code Codex Proxy at user logon.' `
    -Force | Out-Null

Write-Host "Installed startup task '$taskName'."
Write-Host "The proxy will start at logon and serve http://127.0.0.1:4000/"
