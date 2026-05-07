$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe uninstall-startup @args
} else {
    & go run . uninstall-startup @args
}
exit $LASTEXITCODE
