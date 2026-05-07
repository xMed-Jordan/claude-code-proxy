$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe install-startup @args
} else {
    & go run . install-startup @args
}
exit $LASTEXITCODE
