param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Apply', 'Restore', 'BrowserApply', 'BrowserRestore', 'LocalBrowser')]
    [string]$Action
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'
$syncAction = switch ($Action) {
    'BrowserApply' { 'browser-apply' }
    'BrowserRestore' { 'browser-restore' }
    'LocalBrowser' { 'local-browser' }
    default { $Action.ToLowerInvariant() }
}

if (Test-Path -LiteralPath $exe) {
    & $exe sync $syncAction
} else {
    & go run . sync $syncAction
}
exit $LASTEXITCODE
