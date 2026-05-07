$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\claude-code-proxy.exe'

if (Test-Path -LiteralPath $exe) {
    & $exe hook-worktree-create
} else {
    & go run . hook-worktree-create
}
exit $LASTEXITCODE
