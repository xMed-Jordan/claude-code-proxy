param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Apply', 'Restore')]
    [string]$Action
)

$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$settingsPath = Join-Path $env:USERPROFILE '.claude\settings.json'
$settingsDir = Split-Path -Parent $settingsPath
$claudeJsonPath = Join-Path $env:USERPROFILE '.claude.json'
$snapshotPath = Join-Path $basePath '.claude-settings.snapshot.json'
$snapshotMetaPath = Join-Path $basePath '.claude-settings.snapshot.meta.json'
$claudeJsonSnapshotPath = Join-Path $basePath '.claude-json.snapshot.json'
$claudeJsonSnapshotMetaPath = Join-Path $basePath '.claude-json.snapshot.meta.json'
$envFile = Join-Path $basePath '.env'
$gatewayModelsCachePath = Join-Path $env:USERPROFILE '.claude\cache\gateway-models.json'

function Read-ProxyEnv {
    if (-not (Test-Path $envFile)) {
        throw "Missing .env at $envFile."
    }

    $values = @{}
    Get-Content $envFile | ForEach-Object {
        $line = $_.Trim()
        if (-not $line -or $line.StartsWith('#')) { return }
        $idx = $line.IndexOf('=')
        if ($idx -le 0) { return }
        $key = $line.Substring(0, $idx).Trim()
        $value = $line.Substring($idx + 1).Trim()
        $values[$key] = $value
    }
    return $values
}

function Ensure-Snapshot {
    if (Test-Path $snapshotPath) {
        if (-not (Test-Path $snapshotMetaPath)) {
            [pscustomobject]@{
                settings_path = $settingsPath
                existed       = $true
                saved_at      = $null
                reused        = $true
                note          = 'Existing snapshot preserved; not overwritten during proxy start.'
            } | ConvertTo-Json -Depth 4 | Set-Content -Path $snapshotMetaPath -Encoding UTF8
        }
        Write-Host "Existing Claude Code settings snapshot preserved: $snapshotPath"
        return
    }

    if (-not (Test-Path $settingsDir)) {
        New-Item -ItemType Directory -Path $settingsDir -Force | Out-Null
    }

    $existed = Test-Path $settingsPath
    if ($existed) {
        Copy-Item -Path $settingsPath -Destination $snapshotPath -Force
    } else {
        Set-Content -Path $snapshotPath -Value "{}" -Encoding UTF8
    }

    [pscustomobject]@{
        settings_path = $settingsPath
        existed       = $existed
        saved_at      = (Get-Date).ToString('o')
        reused        = $false
    } | ConvertTo-Json -Depth 4 | Set-Content -Path $snapshotMetaPath -Encoding UTF8
    Write-Host "Created Claude Code settings snapshot: $snapshotPath"
}

function Ensure-ClaudeJsonSnapshot {
    if (Test-Path $claudeJsonSnapshotPath) {
        if (-not (Test-Path $claudeJsonSnapshotMetaPath)) {
            [pscustomobject]@{
                config_path = $claudeJsonPath
                existed     = $true
                saved_at    = $null
                reused      = $true
                note        = 'Existing Claude Code root config snapshot preserved; not overwritten during proxy start.'
            } | ConvertTo-Json -Depth 4 | Set-Content -Path $claudeJsonSnapshotMetaPath -Encoding UTF8
        }
        Write-Host "Existing Claude Code root config snapshot preserved: $claudeJsonSnapshotPath"
        return
    }

    $existed = Test-Path $claudeJsonPath
    if ($existed) {
        Copy-Item -Path $claudeJsonPath -Destination $claudeJsonSnapshotPath -Force
    } else {
        Set-Content -Path $claudeJsonSnapshotPath -Value "{}" -Encoding UTF8
    }

    [pscustomobject]@{
        config_path = $claudeJsonPath
        existed     = $existed
        saved_at    = (Get-Date).ToString('o')
        reused      = $false
    } | ConvertTo-Json -Depth 4 | Set-Content -Path $claudeJsonSnapshotMetaPath -Encoding UTF8
    Write-Host "Created Claude Code root config snapshot: $claudeJsonSnapshotPath"
}

function Set-JsonProperty {
    param(
        [Parameter(Mandatory = $true)] [object]$Object,
        [Parameter(Mandatory = $true)] [string]$Name,
        [Parameter(Mandatory = $true)] [object]$Value
    )

    if ($Object.PSObject.Properties.Name -contains $Name) {
        $Object.$Name = $Value
    } else {
        $Object | Add-Member -NotePropertyName $Name -NotePropertyValue $Value
    }
}

function Remove-JsonProperty {
    param(
        [Parameter(Mandatory = $true)] [object]$Object,
        [Parameter(Mandatory = $true)] [string]$Name
    )

    if ($Object.PSObject.Properties.Name -contains $Name) {
        $Object.PSObject.Properties.Remove($Name)
    }
}

function Clear-GatewayModelsCache {
    if (Test-Path $gatewayModelsCachePath) {
        Remove-Item -Path $gatewayModelsCachePath -Force
        Write-Host "Removed stale Claude Code gateway models cache: $gatewayModelsCachePath"
    }
}

function Ensure-AntigravityBrowserMcp {
    param(
        [Parameter(Mandatory = $true)] [object]$Settings
    )

    if (-not ($Settings.PSObject.Properties.Name -contains 'mcpServers') -or $null -eq $Settings.mcpServers) {
        Set-JsonProperty -Object $Settings -Name 'mcpServers' -Value ([pscustomobject]@{})
    }

    $launcherPath = Join-Path $basePath 'start-antigravity-browser-mcp.ps1'
    $server = [pscustomobject]@{
        command = 'pwsh'
        args    = @('-NoProfile', '-File', $launcherPath)
        env     = [pscustomobject]@{
            CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS = '1'
        }
    }

    Set-JsonProperty -Object $Settings.mcpServers -Name 'antigravity-browser' -Value $server
}

function Ensure-AntigravityBrowserUserMcp {
    Ensure-ClaudeJsonSnapshot

    if (Test-Path $claudeJsonPath) {
        $config = Get-Content $claudeJsonPath -Raw | ConvertFrom-Json
    } else {
        $config = [pscustomobject]@{}
    }

    if (-not ($config.PSObject.Properties.Name -contains 'mcpServers') -or $null -eq $config.mcpServers) {
        Set-JsonProperty -Object $config -Name 'mcpServers' -Value ([pscustomobject]@{})
    }

    $launcherPath = Join-Path $basePath 'start-antigravity-browser-mcp.ps1'
    $server = [pscustomobject]@{
        type    = 'stdio'
        command = 'pwsh'
        args    = @('-NoProfile', '-File', $launcherPath)
        env     = [pscustomobject]@{
            CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS = '1'
        }
    }

    Set-JsonProperty -Object $config.mcpServers -Name 'antigravity-browser' -Value $server
    $config | ConvertTo-Json -Depth 100 | Set-Content -Path $claudeJsonPath -Encoding UTF8
    Write-Host "Applied Antigravity MCP to Claude Code root config: $claudeJsonPath"
}

function New-ProxyCommandHook {
    param(
        [Parameter(Mandatory = $true)] [string]$ScriptPath
    )

    return [pscustomobject]@{
        type    = 'command'
        command = "pwsh -NoProfile -ExecutionPolicy Bypass -File `"$ScriptPath`""
    }
}

function Test-HookListContainsScript {
    param(
        [object[]]$HookGroups = @(),
        [Parameter(Mandatory = $true)] [string]$ScriptPath
    )

    foreach ($group in $HookGroups) {
        foreach ($hook in @($group.hooks)) {
            if ($hook.command -and ([string]$hook.command).Contains($ScriptPath)) {
                return $true
            }
        }
    }
    return $false
}

function Ensure-HookEvent {
    param(
        [Parameter(Mandatory = $true)] [object]$Settings,
        [Parameter(Mandatory = $true)] [string]$EventName,
        [Parameter(Mandatory = $true)] [string]$ScriptPath,
        [string]$Matcher = '',
        [switch]$OnlyWhenEmpty
    )

    if (-not ($Settings.PSObject.Properties.Name -contains 'hooks') -or $null -eq $Settings.hooks) {
        Set-JsonProperty -Object $Settings -Name 'hooks' -Value ([pscustomobject]@{})
    }

    $current = @()
    if ($Settings.hooks.PSObject.Properties.Name -contains $EventName -and $null -ne $Settings.hooks.$EventName) {
        $current = @($Settings.hooks.$EventName)
    }

    if (Test-HookListContainsScript -HookGroups $current -ScriptPath $ScriptPath) {
        return
    }

    if ($OnlyWhenEmpty -and $current.Count -gt 0) {
        Write-Host "Existing Claude Code $EventName hook preserved; proxy hook not added."
        return
    }

    $group = [pscustomobject]@{
        hooks = @(New-ProxyCommandHook -ScriptPath $ScriptPath)
    }
    if (-not [string]::IsNullOrWhiteSpace($Matcher)) {
        Set-JsonProperty -Object $group -Name 'matcher' -Value $Matcher
    }

    Set-JsonProperty -Object $Settings.hooks -Name $EventName -Value @($current + $group)
}

function Ensure-ClaudeIsolationHooks {
    param(
        [Parameter(Mandatory = $true)] [object]$Settings
    )

    $worktreeCreatePath = Join-Path $basePath 'claude-worktree-create.ps1'
    $worktreeRemovePath = Join-Path $basePath 'claude-worktree-remove.ps1'
    $subagentContextPath = Join-Path $basePath 'claude-subagent-context.ps1'

    Ensure-HookEvent -Settings $Settings -EventName 'WorktreeCreate' -ScriptPath $worktreeCreatePath -OnlyWhenEmpty
    Ensure-HookEvent -Settings $Settings -EventName 'WorktreeRemove' -ScriptPath $worktreeRemovePath -OnlyWhenEmpty
    Ensure-HookEvent -Settings $Settings -EventName 'SubagentStart' -ScriptPath $subagentContextPath -Matcher '*'
}

function Apply-ProxySettings {
    Ensure-Snapshot
    Clear-GatewayModelsCache

    $proxyEnv = Read-ProxyEnv
    $port = $proxyEnv['PROXY_PORT']
    if ([string]::IsNullOrWhiteSpace($port)) { $port = '4000' }

    $proxyKey = $proxyEnv['PROXY_API_KEY']
    if ([string]::IsNullOrWhiteSpace($proxyKey)) {
        $proxyKey = $proxyEnv['LITELLM_MASTER_KEY']
    }
    if ([string]::IsNullOrWhiteSpace($proxyKey)) {
        throw 'PROXY_API_KEY is not set in .env.'
    }

    if (Test-Path $settingsPath) {
        $settings = Get-Content $settingsPath -Raw | ConvertFrom-Json
    } else {
        $settings = [pscustomobject]@{
            '$schema' = 'https://json.schemastore.org/claude-code-settings.json'
        }
    }

    if (-not ($settings.PSObject.Properties.Name -contains 'env') -or $null -eq $settings.env) {
        Set-JsonProperty -Object $settings -Name 'env' -Value ([pscustomobject]@{})
    }

    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_BASE_URL' -Value "http://127.0.0.1:$port"
    Remove-JsonProperty -Object $settings.env -Name 'ANTHROPIC_API_KEY'
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_AUTH_TOKEN' -Value $proxyKey
    Set-JsonProperty -Object $settings.env -Name 'CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY' -Value '1'
    Set-JsonProperty -Object $settings.env -Name 'CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS' -Value '1'
    $apiTimeoutMs = $proxyEnv['API_TIMEOUT_MS']
    if ([string]::IsNullOrWhiteSpace($apiTimeoutMs)) { $apiTimeoutMs = '3000000' }
    $disableNonessential = $proxyEnv['CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC']
    if ([string]::IsNullOrWhiteSpace($disableNonessential)) { $disableNonessential = '1' }
    $opusDefault = $proxyEnv['ANTHROPIC_DEFAULT_OPUS_MODEL']
    if ([string]::IsNullOrWhiteSpace($opusDefault)) { $opusDefault = 'claude-opus-4-7[1m]' }
    $sonnetDefault = $proxyEnv['ANTHROPIC_DEFAULT_SONNET_MODEL']
    if ([string]::IsNullOrWhiteSpace($sonnetDefault)) { $sonnetDefault = 'claude-sonnet-4-6[1m]' }
    $haikuDefault = $proxyEnv['ANTHROPIC_DEFAULT_HAIKU_MODEL']
    if ([string]::IsNullOrWhiteSpace($haikuDefault)) { $haikuDefault = 'claude-haiku-4-5' }
    $effortLevel = $proxyEnv['CLAUDE_CODE_EFFORT_LEVEL']
    if ([string]::IsNullOrWhiteSpace($effortLevel)) { $effortLevel = 'xhigh' }
    $opusCapabilities = $proxyEnv['ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES']
    if ([string]::IsNullOrWhiteSpace($opusCapabilities)) { $opusCapabilities = 'effort,xhigh_effort,max_effort,thinking,adaptive_thinking,interleaved_thinking' }
    $sonnetCapabilities = $proxyEnv['ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES']
    if ([string]::IsNullOrWhiteSpace($sonnetCapabilities)) { $sonnetCapabilities = 'effort,max_effort,thinking,adaptive_thinking,interleaved_thinking' }
    $haikuCapabilities = $proxyEnv['ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES']
    if ([string]::IsNullOrWhiteSpace($haikuCapabilities)) { $haikuCapabilities = 'thinking' }
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_OPUS_MODEL' -Value $opusDefault
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_SONNET_MODEL' -Value $sonnetDefault
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_HAIKU_MODEL' -Value $haikuDefault
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES' -Value $opusCapabilities
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES' -Value $sonnetCapabilities
    Set-JsonProperty -Object $settings.env -Name 'ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES' -Value $haikuCapabilities
    Set-JsonProperty -Object $settings.env -Name 'CLAUDE_CODE_EFFORT_LEVEL' -Value $effortLevel
    Set-JsonProperty -Object $settings.env -Name 'API_TIMEOUT_MS' -Value $apiTimeoutMs
    Set-JsonProperty -Object $settings.env -Name 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC' -Value $disableNonessential
    Set-JsonProperty -Object $settings -Name 'model' -Value 'opus[1m]'
    Ensure-AntigravityBrowserMcp -Settings $settings
    Ensure-ClaudeIsolationHooks -Settings $settings

    $settings | ConvertTo-Json -Depth 100 | Set-Content -Path $settingsPath -Encoding UTF8
    Ensure-AntigravityBrowserUserMcp
    Write-Host "Applied proxy env to Claude Code settings: $settingsPath"
    Write-Host "Snapshot path: $snapshotPath"
}

function Restore-OriginalSettings {
    if (-not (Test-Path $snapshotPath)) {
        Write-Host 'No Claude Code settings snapshot found to restore.'
        return
    }

    $existed = $true
    if (Test-Path $snapshotMetaPath) {
        $meta = Get-Content $snapshotMetaPath -Raw | ConvertFrom-Json
        $existed = [bool]$meta.existed
    }

    if ($existed) {
        if (-not (Test-Path $settingsDir)) {
            New-Item -ItemType Directory -Path $settingsDir -Force | Out-Null
        }
        Copy-Item -Path $snapshotPath -Destination $settingsPath -Force
        Write-Host "Restored Claude Code settings from snapshot: $settingsPath"
    } else {
        Remove-Item -Path $settingsPath -Force -ErrorAction SilentlyContinue
        Write-Host "Removed Claude Code settings created for proxy: $settingsPath"
    }

    Remove-Item -Path $snapshotPath -Force -ErrorAction SilentlyContinue
    Remove-Item -Path $snapshotMetaPath -Force -ErrorAction SilentlyContinue
}

function Restore-ClaudeJsonConfig {
    if (-not (Test-Path $claudeJsonSnapshotPath)) {
        Write-Host 'No Claude Code root config snapshot found to restore.'
        return
    }

    $existed = $true
    if (Test-Path $claudeJsonSnapshotMetaPath) {
        $meta = Get-Content $claudeJsonSnapshotMetaPath -Raw | ConvertFrom-Json
        $existed = [bool]$meta.existed
    }

    if ($existed) {
        Copy-Item -Path $claudeJsonSnapshotPath -Destination $claudeJsonPath -Force
        Write-Host "Restored Claude Code root config from snapshot: $claudeJsonPath"
    } else {
        Remove-Item -Path $claudeJsonPath -Force -ErrorAction SilentlyContinue
        Write-Host "Removed Claude Code root config created for proxy: $claudeJsonPath"
    }

    Remove-Item -Path $claudeJsonSnapshotPath -Force -ErrorAction SilentlyContinue
    Remove-Item -Path $claudeJsonSnapshotMetaPath -Force -ErrorAction SilentlyContinue
}

if ($Action -eq 'Apply') {
    Apply-ProxySettings
} else {
    Restore-OriginalSettings
    Restore-ClaudeJsonConfig
}
