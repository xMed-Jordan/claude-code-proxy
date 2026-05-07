param(
    [switch]$ValidateOnly,
    [switch]$Stop,
    [switch]$Quiet
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\claude-code-proxy.exe'
if ($ValidateOnly) {
    $command = 'browser-status'
} elseif ($Stop) {
    $command = 'browser-stop'
} else {
    $command = 'browser-start'
}

if (Test-Path -LiteralPath $exe) {
    & $exe $command @args
} else {
    & go run . $command @args
}
exit $LASTEXITCODE
