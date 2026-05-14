$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'
$proxyEnvVars = @('ANTHROPIC_BASE_URL', 'ANTHROPIC_API_KEY')

if (Test-Path -LiteralPath $exe) {
    & $exe sync browser-restore
} else {
    & go run . sync browser-restore
}

foreach ($name in $proxyEnvVars) {
    Remove-Item -Path "Env:\$name" -ErrorAction SilentlyContinue
    Remove-ItemProperty -Path 'HKCU:\Environment' -Name $name -ErrorAction SilentlyContinue
}

Write-Host "Cleared proxy environment variables that can force CLAUDE to use a local endpoint."
exit $LASTEXITCODE
