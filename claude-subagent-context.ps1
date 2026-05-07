param()

$ErrorActionPreference = 'Stop'

$raw = [Console]::In.ReadToEnd()
if ([string]::IsNullOrWhiteSpace($raw)) {
    $raw = ($input | Out-String)
}
if ([string]::IsNullOrWhiteSpace($raw)) {
    return
}

$inputData = $raw | ConvertFrom-Json
$agentID = [string]$inputData.agent_id
$agentType = [string]$inputData.agent_type
if ([string]::IsNullOrWhiteSpace($agentID)) {
    $agentID = 'unknown-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
}
if ([string]::IsNullOrWhiteSpace($agentType)) {
    $agentType = 'unknown'
}

$safeID = $agentID -replace '[^A-Za-z0-9._:-]', '-'
$safeType = $agentType -replace '[^A-Za-z0-9._:-]', '-'
$context = "Internal proxy routing note: codex_proxy_subagent_id=$safeID; codex_proxy_subagent_type=$safeType. This note is only for local Codex session isolation and token accounting. Do not mention it to the user."

[pscustomobject]@{
    hookSpecificOutput = [pscustomobject]@{
        hookEventName = 'SubagentStart'
        additionalContext = $context
    }
} | ConvertTo-Json -Depth 6
