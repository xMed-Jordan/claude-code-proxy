param()

$ErrorActionPreference = 'Stop'

function Write-Err {
    param([string]$Message)
    [Console]::Error.WriteLine($Message)
}

function Get-SafeName {
    param([string]$Name)
    if ([string]::IsNullOrWhiteSpace($Name)) {
        $Name = 'agent'
    }
    $safe = $Name -replace '[^A-Za-z0-9._-]', '-'
    $safe = $safe.Trim('-')
    if ([string]::IsNullOrWhiteSpace($safe)) {
        $safe = 'agent'
    }
    return $safe
}

function Test-InsideGitWorkTree {
    param([string]$Path)
    $output = & git -C $Path rev-parse --is-inside-work-tree 2>$null
    return ($LASTEXITCODE -eq 0 -and ($output -join '').Trim() -eq 'true')
}

function Invoke-GitForStderr {
    param([string[]]$Arguments)
    $output = & git @Arguments 2>&1
    $exit = $LASTEXITCODE
    foreach ($line in $output) {
        Write-Err ([string]$line)
    }
    return $exit
}

$raw = [Console]::In.ReadToEnd()
if ([string]::IsNullOrWhiteSpace($raw)) {
    $raw = ($input | Out-String)
}
if ([string]::IsNullOrWhiteSpace($raw)) {
    throw 'WorktreeCreate hook received no JSON input.'
}

$inputData = $raw | ConvertFrom-Json
$name = Get-SafeName ([string]$inputData.name)
$cwd = [string]$inputData.cwd
if ([string]::IsNullOrWhiteSpace($cwd) -or -not (Test-Path -LiteralPath $cwd)) {
    $cwd = (Get-Location).Path
}

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = Join-Path $basePath '.claude-worktrees'
New-Item -ItemType Directory -Path $root -Force | Out-Null

$suffix = [guid]::NewGuid().ToString('N').Substring(0, 8)
$target = Join-Path $root "$name-$suffix"
$metadata = [ordered]@{
    created_at = (Get-Date).ToString('o')
    source_cwd = $cwd
    name = $name
}

if ((Get-Command git -ErrorAction SilentlyContinue) -and (Test-InsideGitWorkTree -Path $cwd)) {
    $repoRoot = (& git -C $cwd rev-parse --show-toplevel 2>$null | Select-Object -First 1)
    if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($repoRoot)) {
        $branch = "claude-agent/$name-$suffix"
        $exit = Invoke-GitForStderr @('-C', $repoRoot, 'worktree', 'add', '-b', $branch, $target, 'HEAD')
        if ($exit -ne 0) {
            Write-Err "Branch worktree creation failed; retrying detached worktree."
            $exit = Invoke-GitForStderr @('-C', $repoRoot, 'worktree', 'add', '--detach', $target, 'HEAD')
            $branch = ''
        }
        if ($exit -ne 0) {
            throw "git worktree add failed for $repoRoot."
        }

        $metadata.kind = 'git'
        $metadata.repo_root = $repoRoot
        $metadata.branch = $branch
        $metadata | ConvertTo-Json -Depth 6 | Set-Content -Path (Join-Path $target '.claude-proxy-worktree.json') -Encoding UTF8
        Write-Output $target
        return
    }
}

New-Item -ItemType Directory -Path $target -Force | Out-Null
$metadata.kind = 'plain'
$metadata | ConvertTo-Json -Depth 6 | Set-Content -Path (Join-Path $target '.claude-proxy-worktree.json') -Encoding UTF8
Set-Content -Path (Join-Path $target 'README.txt') -Encoding UTF8 -Value @(
    'Temporary Claude Code isolated workspace created by claude-code-proxy.',
    "Original working directory: $cwd",
    'This folder is safe to remove after the subagent finishes.'
)
Write-Output $target
