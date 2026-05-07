param(
    [switch]$ValidateOnly
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'
$command = if ($ValidateOnly) { 'browser-status' } else { 'browser-mcp' }

if (Test-Path -LiteralPath $exe) {
    & $exe $command @args
} else {
    & go run . $command @args
}
exit $LASTEXITCODE
