param(
    [switch]$ValidateOnly
)

$ErrorActionPreference = 'Stop'

$callerPath = (Get-Location).Path
$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable('ANTIGRAVITY_BROWSER_CALLER_CWD', 'Process')) -and -not [string]::IsNullOrWhiteSpace($callerPath)) {
    [Environment]::SetEnvironmentVariable('ANTIGRAVITY_BROWSER_CALLER_CWD', $callerPath, 'Process')
}
Set-Location $basePath
$extensionId = 'eeijfnjmjelapkebgockoeaadonbchdd'

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

$exePath = Join-Path $basePath 'bin\claude-code-proxy.exe'
$chromePath = Get-ChromePath
$extensionPath = Get-AntigravityExtensionPath
$browserMode = Get-BrowserMode
$debugPort = Get-DebugPort
$browserUrl = "http://127.0.0.1:$debugPort"
$browserRunning = Test-BrowserEndpoint -Port $debugPort

$problems = @()
if (-not (Test-Path -LiteralPath $exePath)) {
    $problems += "Proxy executable was not found at $exePath. Build the project first."
}
if ([string]::IsNullOrWhiteSpace($chromePath)) {
    $problems += 'Google Chrome was not found. Set ANTIGRAVITY_CHROME_PATH to chrome.exe if it is installed in a custom location.'
}
if ([string]::IsNullOrWhiteSpace($extensionPath)) {
    $problems += "Antigravity extension $extensionId was not found in the Chrome Default profile. Install it once in Chrome, or set ANTIGRAVITY_EXTENSION_PATH."
}

if ($ValidateOnly) {
    [pscustomobject]@{
        ok              = ($problems.Count -eq 0)
        mode            = $browserMode
        executable_path = $exePath
        chrome_path     = $chromePath
        extension_id    = $extensionId
        extension_path  = $extensionPath
        browser_url     = $browserUrl
        browser_running = $browserRunning
        visible_overlay = $true
        mcp_server      = 'custom'
        problems        = $problems
    } | ConvertTo-Json -Depth 4
    if ($problems.Count -eq 0) { exit 0 }
    exit 1
}

if ($problems.Count -gt 0) {
    foreach ($problem in $problems) {
        [Console]::Error.WriteLine($problem)
    }
    exit 1
}

& $exePath --antigravity-mcp
exit $LASTEXITCODE
