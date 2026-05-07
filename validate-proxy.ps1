$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath

if (-not (Test-Path '.env')) {
    throw 'Missing .env. Copy .env.example to .env and set OPENAI_API_KEY and PROXY_API_KEY.'
}

Get-Content '.env' | ForEach-Object {
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
$baseUrl = "http://127.0.0.1:$port"
$anthropicBaseUrl = "$baseUrl/anthropic"

$proxyKey = [Environment]::GetEnvironmentVariable('PROXY_API_KEY', 'Process')
$headers = @{
    'Content-Type' = 'application/json'
    'anthropic-version' = '2023-06-01'
}
if (-not [string]::IsNullOrWhiteSpace($proxyKey)) {
    $headers['x-api-key'] = $proxyKey
}

$models = Invoke-RestMethod -Method Get -Uri "$anthropicBaseUrl/v1/models" -Headers $headers
Write-Host 'GET /anthropic/v1/models: OK'
Write-Host ($models | ConvertTo-Json -Depth 6)

$tokenBody = @{
    model = 'claude-3-7-sonnet-latest'
    messages = @(@{ role = 'user'; content = 'Quick count test' })
} | ConvertTo-Json -Depth 8
$tokenResp = Invoke-RestMethod -Method Post -Uri "$anthropicBaseUrl/v1/messages/count_tokens" -Headers $headers -Body $tokenBody
Write-Host 'POST /anthropic/v1/messages/count_tokens: OK'
Write-Host ($tokenResp | ConvertTo-Json -Depth 6)

$upstream = [Environment]::GetEnvironmentVariable('UPSTREAM', 'Process')
$apiKey = [Environment]::GetEnvironmentVariable('OPENAI_API_KEY', 'Process')
if ($upstream -ne 'codex' -and ($apiKey -eq 'YOUR_OPENAI_API_KEY' -or $apiKey -like 'sk-or-*' -or [string]::IsNullOrWhiteSpace($apiKey))) {
    Write-Host 'Skipping POST /anthropic/v1/messages because OPENAI_API_KEY is a placeholder and UPSTREAM is not codex.'
    exit 0
}

$messagesBody = @{
    model = 'claude-3-7-sonnet-latest'
    max_tokens = 32
    messages = @(@{ role = 'user'; content = 'Say hello in one sentence.' })
} | ConvertTo-Json -Depth 8
$messageResp = Invoke-RestMethod -Method Post -Uri "$anthropicBaseUrl/v1/messages" -Headers $headers -Body $messagesBody
Write-Host 'POST /anthropic/v1/messages: OK'
Write-Host ($messageResp | ConvertTo-Json -Depth 8)

$streamBody = @{
    model = 'claude-3-7-sonnet-latest'
    max_tokens = 32
    stream = $true
    messages = @(@{ role = 'user'; content = 'Say streaming hello in two words.' })
} | ConvertTo-Json -Depth 8
$streamResp = Invoke-WebRequest -Method Post -Uri "$anthropicBaseUrl/v1/messages" -Headers $headers -Body $streamBody
if ($streamResp.Content -notmatch 'message_start' -or $streamResp.Content -notmatch 'content_block_delta' -or $streamResp.Content -notmatch 'message_stop') {
    throw 'POST /anthropic/v1/messages streaming response did not include expected Anthropic SSE events.'
}
Write-Host 'POST /anthropic/v1/messages stream: OK'
