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

function Test-ForceDefaultCdp {
    $flag = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP', 'Process')
    return (-not [string]::IsNullOrWhiteSpace($flag) -and $flag -match '^(1|true|yes|on)$')
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

    $candidateRoots = @(
        $env:ProgramFiles,
        ${env:ProgramFiles(x86)},
        $env:LOCALAPPDATA
    )
    $candidates = @()
    foreach ($root in $candidateRoots) {
        if (-not [string]::IsNullOrWhiteSpace($root)) {
            $candidates += (Join-Path $root 'Google\Chrome\Application\chrome.exe')
        }
    }
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

function Test-ChromeProcessRunning {
    return $null -ne (Get-Process -Name chrome -ErrorAction SilentlyContinue | Select-Object -First 1)
}

function Get-ChromeProcessInfo {
    @(Get-Process -Name chrome -ErrorAction SilentlyContinue | ForEach-Object {
        [pscustomobject]@{
            Id        = $_.Id
            Title     = [string]$_.MainWindowTitle
            IsVisible = ($_.MainWindowHandle -ne 0)
        }
    })
}

function Test-SafeChromeWindowTitle {
    param([string]$Title)
    $normalized = $Title.Trim().ToLowerInvariant()
    if ([string]::IsNullOrWhiteSpace($normalized)) {
        return $true
    }
    if ($normalized -eq 'about:blank - google chrome' -or $normalized -eq 'new tab - google chrome' -or $normalized -eq 'olemainthreadwndname') {
        return $true
    }
    foreach ($part in @('claude code codex proxy', 'codex proxy control panel', '127.0.0.1:4000', 'localhost:4000')) {
        if ($normalized.Contains($part)) {
            return $true
        }
    }
    return $false
}

function Test-CanRelaunchDefaultChrome {
    $flag = [Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH', 'Process')
    if (-not [string]::IsNullOrWhiteSpace($flag) -and $flag -match '^(0|false|no|off)$') {
        return $false
    }
    $visible = @(Get-ChromeProcessInfo | Where-Object { $_.IsVisible })
    if ($visible.Count -eq 0) {
        return $true
    }
    foreach ($item in $visible) {
        if (-not (Test-SafeChromeWindowTitle -Title $item.Title)) {
            return $false
        }
    }
    return $true
}

function Stop-ChromeProcessesForDefaultBridge {
    taskkill.exe /IM chrome.exe /F /T *> $null
    $deadline = (Get-Date).AddSeconds(3)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-ChromeProcessRunning)) {
            return $true
        }
        Start-Sleep -Milliseconds 250
    }
    return (-not (Test-ChromeProcessRunning))
}

function Stop-AntigravityBrowser {
    if ((Get-BrowserMode) -eq 'default' -and -not (Test-Path -LiteralPath $pidFile)) {
        Write-Info 'Default browser mode is active and no controlled Antigravity browser PID file exists.'
        return
    }

    $profilePath = Get-BrowserProfilePath
    if (-not (Test-Path -LiteralPath $pidFile)) {
        Write-Info 'No Antigravity browser PID file found.'
        return
    }

    try {
        $browserPid = [int](Get-Content -Path $pidFile -Raw).Trim()
        $proc = Get-Process -Id $browserPid -ErrorAction SilentlyContinue
        if ($null -eq $proc) {
            Write-Info "No running Antigravity browser process found for PID $browserPid."
            return
        }

        $commandLine = ''
        try {
            $cim = Get-CimInstance Win32_Process -Filter "ProcessId=$browserPid" -ErrorAction Stop
            $commandLine = [string]$cim.CommandLine
        } catch {
            $commandLine = ''
        }

        if ($commandLine -and -not $commandLine.Contains($profilePath)) {
            Write-Info "Refusing to stop PID $browserPid because it does not use the Antigravity profile."
            return
        }

        Stop-Process -Id $browserPid -Force
        Write-Info "Stopped Antigravity browser (PID $browserPid)."
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
        default_cdp_forced = Test-ForceDefaultCdp
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
    if ($running) {
        Write-Info "Default Chrome already exposes DevTools at $browserUrl."
        return
    }

    $effectiveMode = 'default'
    if (-not (Test-ForceDefaultCdp)) {
        $effectiveMode = 'dedicated_fallback'
        Write-Info 'Modern Chrome blocks DevTools control on the normal Default profile. Starting a visible controlled Chrome profile for Claude Code browser actions.'
    } elseif (Test-ChromeProcessRunning) {
        if (Test-CanRelaunchDefaultChrome) {
            Write-Info "Chrome is running only in the background or on the proxy dashboard; relaunching the Default profile with DevTools at $browserUrl."
            if (-not (Stop-ChromeProcessesForDefaultBridge)) {
                Write-Info 'Could not stop existing Chrome processes; starting a visible isolated Chrome fallback for Claude Code browser control.'
                $effectiveMode = 'dedicated_fallback'
            }
        } else {
            $effectiveMode = 'dedicated_fallback'
            Write-Info "Default Chrome has user windows open without DevTools at $browserUrl. Starting a visible isolated Chrome fallback for Claude Code browser control."
        }
    }

    if ($effectiveMode -eq 'dedicated_fallback') {
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
    } else {
        $chromeArgs = @(
            "--remote-debugging-address=127.0.0.1",
            "--remote-debugging-port=$debugPort",
            '--profile-directory=Default',
            '--no-first-run',
            '--no-default-browser-check',
            'about:blank'
        )
    }

    $process = Start-Process -FilePath $chromePath -ArgumentList $chromeArgs -PassThru
    Set-Content -Path $pidFile -Value $process.Id
    Start-Sleep -Milliseconds 800

    if (Test-BrowserEndpoint -Port $debugPort) {
        Write-Info "Started Chrome browser control at $browserUrl (mode $effectiveMode, PID $($process.Id))."
    } else {
        Write-Info "Chrome opened, but DevTools is not responding at $browserUrl. Modern Chrome requires a non-default data directory for browser control unless ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1 works on this Chrome build."
    }
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

$process = Start-Process -FilePath $chromePath -ArgumentList $chromeArgs -PassThru
Set-Content -Path $pidFile -Value $process.Id
Start-Sleep -Milliseconds 800

if (Test-BrowserEndpoint -Port $debugPort) {
    Write-Info "Started Antigravity browser at $browserUrl (PID $($process.Id))."
} else {
    Write-Info "Antigravity browser was started but is not responding at $browserUrl yet."
}
