param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Apply', 'Restore')]
    [string]$Action
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'
$syncAction = $Action.ToLowerInvariant()

if (Test-Path -LiteralPath $exe) {
    & $exe sync $syncAction
} else {
    & go run . sync $syncAction
}
exit $LASTEXITCODE
