$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath

$envFile = Join-Path $basePath '.env'
if (-not (Test-Path $envFile)) {
    throw 'Missing .env. Copy .env.example to .env. For Codex subscription auth, run codex login --device-auth and set PROXY_API_KEY.'
}

Get-Content $envFile | ForEach-Object {
    $line = $_.Trim()
    if (-not $line -or $line.StartsWith('#')) { return }
    $idx = $line.IndexOf('=')
    if ($idx -le 0) { return }
    $key = $line.Substring(0, $idx)
    $value = $line.Substring($idx + 1)
    [Environment]::SetEnvironmentVariable($key, $value, 'Process')
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is not available in PATH.'
}

$port = [Environment]::GetEnvironmentVariable('PROXY_PORT', 'Process')
if ([string]::IsNullOrWhiteSpace($port)) { $port = '4000' }

& (Join-Path $basePath 'sync-claude-settings.ps1') -Action Apply

$startAntigravity = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_START_WITH_WINDOWS', 'Process')
if ($startAntigravity -match '^(1|true|yes|on)$') {
    & (Join-Path $basePath 'start-antigravity-browser.ps1') -Quiet
}

$pidFile = Join-Path $basePath '.proxy.pid'
if (Test-Path $pidFile) {
    try {
        $existingPid = [int](Get-Content $pidFile -Raw).Trim()
        if (Get-Process -Id $existingPid -ErrorAction SilentlyContinue) {
            Write-Host "Go proxy already running with PID $existingPid."
            exit 0
        }
    } finally {
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    }
}

try {
    $health = Invoke-RestMethod -Uri "http://127.0.0.1:$port/health" -TimeoutSec 2 -ErrorAction Stop
    if ($health.ok -eq $true) {
        Write-Host "Go proxy already responding on http://127.0.0.1:$port."
        exit 0
    }
} catch {
    # No responsive proxy found; continue with build/start.
}

$binDir = Join-Path $basePath 'bin'
if (-not (Test-Path $binDir)) {
    New-Item -ItemType Directory -Path $binDir | Out-Null
}

$exe = Join-Path $binDir 'claude-code-proxy.exe'
Write-Host 'Building Go proxy...'
& go build -o $exe .
if ($LASTEXITCODE -ne 0) {
    throw 'Go build failed.'
}

$logFile = Join-Path $basePath '.proxy.log'
$errorLogFile = Join-Path $basePath '.proxy.err.log'
Write-Host "Starting Go proxy on http://127.0.0.1:$port"
$process = Start-Process -FilePath $exe -WorkingDirectory $basePath -PassThru -WindowStyle Hidden -RedirectStandardOutput $logFile -RedirectStandardError $errorLogFile
Set-Content -Path $pidFile -Value $process.Id
Start-Sleep -Milliseconds 500
if ($process.HasExited) {
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    throw "Go proxy exited during startup. See $errorLogFile."
}
Write-Host "Started Go proxy (PID $($process.Id))."
Write-Host "Use this URL in Claude Code: http://127.0.0.1:$port"
