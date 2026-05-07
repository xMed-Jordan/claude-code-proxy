$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$envFile = Join-Path $basePath '.env'
if (-not (Test-Path $envFile)) {
    throw 'Missing .env. Run start-proxy.ps1 from this directory first.'
}

Get-Content $envFile | ForEach-Object {
    $line = $_.Trim()
    if (-not $line -or $line.StartsWith('#')) { return }
    $idx = $line.IndexOf('=')
    if ($idx -le 0) { return }
    $key = $line.Substring(0, $idx)
    $value = $line.Substring($idx + 1)
    [Environment]::SetEnvironmentVariable($key, $value, 'Process')
}

$port = [Environment]::GetEnvironmentVariable('PROXY_PORT', 'Process')
if ([string]::IsNullOrWhiteSpace($port)) { $port = '4000' }
$proxyKey = [Environment]::GetEnvironmentVariable('PROXY_API_KEY', 'Process')
if ([string]::IsNullOrWhiteSpace($proxyKey)) {
    throw 'PROXY_API_KEY is not set in .env.'
}

& (Join-Path $basePath 'start-proxy.ps1')

$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:$port"
Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue
$env:ANTHROPIC_AUTH_TOKEN = $proxyKey
$env:CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY = '1'
$env:CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS = '1'
$env:API_TIMEOUT_MS = '3000000'
$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = '1'
$env:ANTHROPIC_DEFAULT_OPUS_MODEL = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_OPUS_MODEL', 'Process')
$env:ANTHROPIC_DEFAULT_SONNET_MODEL = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_SONNET_MODEL', 'Process')
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_HAIKU_MODEL', 'Process')
$env:ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES', 'Process')
$env:ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES', 'Process')
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES = [Environment]::GetEnvironmentVariable('ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES', 'Process')
$env:CLAUDE_CODE_EFFORT_LEVEL = [Environment]::GetEnvironmentVariable('CLAUDE_CODE_EFFORT_LEVEL', 'Process')

Write-Host "Starting Claude Code through local Codex proxy at $env:ANTHROPIC_BASE_URL"
Write-Host 'Default model aliases exposed by proxy: opus[1m], sonnet[1m], haiku, gpt-5.3-codex'
& claude @args
