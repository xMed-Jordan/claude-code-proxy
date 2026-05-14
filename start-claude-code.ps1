$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath

if (Get-Command claude -ErrorAction SilentlyContinue) {
    claude @args
    exit $LASTEXITCODE
}

Write-Host 'Native "claude" CLI is not installed on PATH. Install/enable Claude Code first if you want native subscription routing.'
Write-Host 'Current anti-gravity MCP can still work if you launch via Claude Desktop.'
exit 1
