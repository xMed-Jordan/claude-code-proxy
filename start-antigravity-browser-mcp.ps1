param(
    [switch]$ValidateOnly
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$profileDir = Join-Path $basePath '.antigravity-browser-profile'
$extensionId = 'eeijfnjmjelapkebgockoeaadonbchdd'

function Write-McpError {
    param([string]$Message)
    [Console]::Error.WriteLine($Message)
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

function Get-NpxPath {
    foreach ($name in @('npx.cmd', 'npx.exe', 'npx')) {
        $cmd = Get-Command $name -ErrorAction SilentlyContinue
        if ($cmd) {
            return $cmd.Source
        }
    }
    return $null
}

$chromePath = Get-ChromePath
$extensionPath = Get-AntigravityExtensionPath
$npxPath = Get-NpxPath

$problems = @()
if ([string]::IsNullOrWhiteSpace($chromePath)) {
    $problems += 'Google Chrome was not found. Set ANTIGRAVITY_CHROME_PATH to chrome.exe if it is installed in a custom location.'
}
if ([string]::IsNullOrWhiteSpace($extensionPath)) {
    $problems += "Antigravity extension $extensionId was not found in the Chrome Default profile. Install it once in Chrome, or set ANTIGRAVITY_EXTENSION_PATH."
}
if ([string]::IsNullOrWhiteSpace($npxPath)) {
    $problems += 'npx was not found on PATH. Install Node.js 20+ or add npm to PATH.'
}

if ($ValidateOnly) {
    $exitCode = 1
    if ($problems.Count -eq 0) {
        $exitCode = 0
    }
    [pscustomobject]@{
        ok             = ($problems.Count -eq 0)
        chrome_path    = $chromePath
        extension_id   = $extensionId
        extension_path = $extensionPath
        profile_path   = $profileDir
        npx_path       = $npxPath
        problems       = $problems
    } | ConvertTo-Json -Depth 4
    exit $exitCode
}

if ($problems.Count -gt 0) {
    foreach ($problem in $problems) {
        Write-McpError $problem
    }
    exit 1
}

if (-not (Test-Path -LiteralPath $profileDir)) {
    New-Item -ItemType Directory -Path $profileDir -Force | Out-Null
}

$env:CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS = '1'

$chromeArgs = @(
    "--load-extension=$extensionPath",
    "--disable-extensions-except=$extensionPath",
    '--no-first-run',
    '--no-default-browser-check'
)

$mcpArgs = @(
    '-y',
    'chrome-devtools-mcp@latest',
    "--executable-path=$chromePath",
    "--user-data-dir=$profileDir",
    '--category-extensions=true',
    '--redact-network-headers=true',
    '--usage-statistics=false',
    '--performance-crux=false'
)

foreach ($arg in $chromeArgs) {
    $mcpArgs += "--chrome-arg=$arg"
}

& $npxPath @mcpArgs
exit $LASTEXITCODE
