$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe hook-subagent-context
} else {
    & go run . hook-subagent-context
}
exit $LASTEXITCODE
