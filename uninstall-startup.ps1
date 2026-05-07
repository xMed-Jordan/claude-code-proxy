$ErrorActionPreference = 'Stop'

$taskName = 'ClaudeCodeCodexProxy'
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if ($null -eq $task) {
    Write-Host "Startup task '$taskName' is not installed."
    exit 0
}

Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
Write-Host "Removed startup task '$taskName'."
