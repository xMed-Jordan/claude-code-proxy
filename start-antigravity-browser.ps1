param(
    [switch]$ValidateOnly,
    [switch]$Stop,
    [switch]$Quiet
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$extensionId = 'eeijfnjmjelapkebgockoeaadonbchdd'
$pidFile = Join-Path $basePath '.antigravity-browser.pid'

function Import-ProxyEnv {
    $envFile = Join-Path $basePath '.env'
    if (-not (Test-Path -LiteralPath $envFile)) {
        return
    }
    Get-Content -Path $envFile | ForEach-Object {
        $line = $_.Trim()
        if (-not $line -or $line.StartsWith('#')) { return }
        $idx = $line.IndexOf('=')
        if ($idx -le 0) { return }
        $key = $line.Substring(0, $idx).Trim()
        $value = $line.Substring($idx + 1).Trim()
        if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($key, 'Process'))) {
            [Environment]::SetEnvironmentVariable($key, $value, 'Process')
        }
    }
}

Import-ProxyEnv

function Write-Info {
    param([string]$Message)
    if (-not $Quiet) {
        Write-Host $Message
    }
}

function Get-BrowserProfilePath {
    $envPath = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_PROFILE', 'Process')
    if (-not [string]::IsNullOrWhiteSpace($envPath)) {
        return $envPath
    }
    return (Join-Path $basePath '.antigravity-browser-profile')
}

function Get-BrowserMode {
    $mode = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_MODE', 'Process')
    if ([string]::IsNullOrWhiteSpace($mode)) {
        if ([Environment]::GetEnvironmentVariable('ANTIGRAVITY_USE_DEFAULT_BROWSER', 'Process') -match '^(1|true|yes|on)$') {
            return 'default'
        }
        return 'dedicated'
    }
    $mode = $mode.Trim().ToLowerInvariant()
    if ($mode -in @('default', 'existing', 'normal')) {
        return 'default'
    }
    return 'dedicated'
}

function Get-DebugPort {
    $value = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_DEBUG_PORT', 'Process')
    if ([string]::IsNullOrWhiteSpace($value)) {
        return 9233
    }
    $port = 0
    if ([int]::TryParse($value, [ref]$port) -and $port -gt 0 -and $port -lt 65536) {
        return $port
    }
    return 9233
}

function Get-ChromePath {
    $envPath = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_CHROME_PATH', 'Process')
    if (-not [string]::IsNullOrWhiteSpace($envPath) -and (Test-Path -LiteralPath $envPath)) {
        return (Resolve-Path -LiteralPath $envPath).Path
    }

    $candidates = @(
        (Join-Path $env:ProgramFiles 'Google\Chrome\Application\chrome.exe'),
        (Join-Path ${env:ProgramFiles(x86)} 'Google\Chrome\Application\chrome.exe'),
        (Join-Path $env:LOCALAPPDATA 'Google\Chrome\Application\chrome.exe')
    )
    foreach ($candidate in $candidates) {
        if (-not [string]::IsNullOrWhiteSpace($candidate) -and (Test-Path -LiteralPath $candidate)) {
            return (Resolve-Path -LiteralPath $candidate).Path
        }
    }
    return $null
}

function Get-AntigravityExtensionPath {
    $envPath = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_EXTENSION_PATH', 'Process')
    if (-not [string]::IsNullOrWhiteSpace($envPath) -and (Test-Path -LiteralPath (Join-Path $envPath 'manifest.json'))) {
        return (Resolve-Path -LiteralPath $envPath).Path
    }

    $root = Join-Path $env:LOCALAPPDATA "Google\Chrome\User Data\Default\Extensions\$extensionId"
    if (-not (Test-Path -LiteralPath $root)) {
        return $null
    }

    $versions = Get-ChildItem -LiteralPath $root -Directory -ErrorAction SilentlyContinue |
        Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'manifest.json') } |
        Sort-Object LastWriteTime -Descending
    if ($versions.Count -eq 0) {
        return $null
    }
    return $versions[0].FullName
}

function Test-BrowserEndpoint {
    param([int]$Port)
    try {
        $version = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/json/version" -TimeoutSec 2 -ErrorAction Stop
        return ($null -ne $version.webSocketDebuggerUrl)
    } catch {
        return $false
    }
}

function Stop-AntigravityBrowser {
    if ((Get-BrowserMode) -eq 'default') {
        Write-Info 'Default browser mode is active; no dedicated Antigravity browser is stopped.'
        Remove-Item -Path $pidFile -Force -ErrorAction SilentlyContinue
        return
    }

    $profilePath = Get-BrowserProfilePath
    if (-not (Test-Path -LiteralPath $pidFile)) {
        Write-Info 'No Antigravity browser PID file found.'
        return
    }

    try {
        $pid = [int](Get-Content -Path $pidFile -Raw).Trim()
        $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue
        if ($null -eq $proc) {
            Write-Info "No running Antigravity browser process found for PID $pid."
            return
        }

        $commandLine = ''
        try {
            $cim = Get-CimInstance Win32_Process -Filter "ProcessId=$pid" -ErrorAction Stop
            $commandLine = [string]$cim.CommandLine
        } catch {
            $commandLine = ''
        }

        if ($commandLine -and -not $commandLine.Contains($profilePath)) {
            Write-Info "Refusing to stop PID $pid because it does not use the Antigravity profile."
            return
        }

        Stop-Process -Id $pid -Force
        Write-Info "Stopped Antigravity browser (PID $pid)."
    } finally {
        Remove-Item -Path $pidFile -Force -ErrorAction SilentlyContinue
    }
}

$profileDir = Get-BrowserProfilePath
$browserMode = Get-BrowserMode
$debugPort = Get-DebugPort
$browserUrl = "http://127.0.0.1:$debugPort"
$chromePath = Get-ChromePath
$extensionPath = Get-AntigravityExtensionPath

if ($Stop) {
    Stop-AntigravityBrowser
    return
}

$configured = (-not [string]::IsNullOrWhiteSpace($extensionPath))
$ready = $configured -and (-not [string]::IsNullOrWhiteSpace($chromePath))
$running = Test-BrowserEndpoint -Port $debugPort

if ($ValidateOnly) {
    [pscustomobject]@{
        ok             = $ready
        mode           = $browserMode
        configured     = $configured
        running        = $running
        browser_url    = $browserUrl
        chrome_path    = $chromePath
        extension_id   = $extensionId
        extension_path = $extensionPath
        profile_path   = $profileDir
        pid_file       = $pidFile
    } | ConvertTo-Json -Depth 4
    if ($ready) { exit 0 }
    exit 1
}

if (-not $configured) {
    Write-Info "Antigravity extension $extensionId is not configured; startup browser skipped."
    return
}
if ([string]::IsNullOrWhiteSpace($chromePath)) {
    Write-Info 'Google Chrome was not found; Antigravity startup browser skipped.'
    return
}
if ($browserMode -eq 'default') {
    Write-Info 'Default browser mode is active; Claude Code will auto-connect to your regular Chrome when the MCP starts.'
    return
}
if ($running) {
    Write-Info "Antigravity browser already responds at $browserUrl."
    return
}

if (-not (Test-Path -LiteralPath $profileDir)) {
    New-Item -ItemType Directory -Path $profileDir -Force | Out-Null
}

$chromeArgs = @(
    "--remote-debugging-address=127.0.0.1",
    "--remote-debugging-port=$debugPort",
    "--user-data-dir=$profileDir",
    "--load-extension=$extensionPath",
    "--disable-extensions-except=$extensionPath",
    '--no-first-run',
    '--no-default-browser-check',
    'about:blank'
)

$process = Start-Process -FilePath $chromePath -ArgumentList $chromeArgs -PassThru -WindowStyle Hidden
Set-Content -Path $pidFile -Value $process.Id
Start-Sleep -Milliseconds 800

if (Test-BrowserEndpoint -Port $debugPort) {
    Write-Info "Started Antigravity browser at $browserUrl (PID $($process.Id))."
} else {
    Write-Info "Antigravity browser was started but is not responding at $browserUrl yet."
}
