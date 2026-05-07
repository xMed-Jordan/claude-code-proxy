$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe sync browser-restore
} else {
    & go run . sync browser-restore
}
exit $LASTEXITCODE
