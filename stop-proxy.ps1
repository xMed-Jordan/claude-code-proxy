$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$pidFile = Join-Path $basePath '.proxy.pid'

function Restore-ClaudeSettings {
    & (Join-Path $basePath 'sync-claude-settings.ps1') -Action Restore
}

if (-not (Test-Path $pidFile)) {
    Write-Host 'No .proxy.pid file found.'
    Restore-ClaudeSettings
    exit 0
}

try {
    $proxyPid = [int](Get-Content $pidFile -Raw).Trim()
    $proc = Get-Process -Id $proxyPid -ErrorAction SilentlyContinue
    if ($null -ne $proc) {
        Stop-Process -Id $proxyPid -Force
        Write-Host "Stopped Go proxy (PID $proxyPid)."
    } else {
        Write-Host "No running process found for PID $proxyPid."
    }
} finally {
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    Restore-ClaudeSettings
}
