$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\claude-code-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe hook-subagent-context
} else {
    & go run . hook-subagent-context
}
exit $LASTEXITCODE
