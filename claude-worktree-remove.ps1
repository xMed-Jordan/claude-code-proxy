param()

$ErrorActionPreference = 'Stop'

function Write-Err {
    param([string]$Message)
    [Console]::Error.WriteLine($Message)
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
    return
}

$inputData = $raw | ConvertFrom-Json
$worktreePath = [string]$inputData.worktree_path
if ([string]::IsNullOrWhiteSpace($worktreePath)) {
    return
}

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = [System.IO.Path]::GetFullPath((Join-Path $basePath '.claude-worktrees'))
$target = [System.IO.Path]::GetFullPath($worktreePath)

if (-not $target.StartsWith($root, [System.StringComparison]::OrdinalIgnoreCase)) {
    Write-Err "Refusing to remove worktree outside proxy root: $target"
    return
}

if (-not (Test-Path -LiteralPath $target)) {
    return
}

$metadataPath = Join-Path $target '.claude-proxy-worktree.json'
$metadata = $null
if (Test-Path -LiteralPath $metadataPath) {
    $metadata = Get-Content -Path $metadataPath -Raw | ConvertFrom-Json
}

if ($metadata -and $metadata.kind -eq 'git' -and $metadata.repo_root -and (Get-Command git -ErrorAction SilentlyContinue)) {
    $exit = Invoke-GitForStderr @('-C', [string]$metadata.repo_root, 'worktree', 'remove', '--force', $target)
    if ($exit -eq 0 -and $metadata.branch) {
        [void](Invoke-GitForStderr @('-C', [string]$metadata.repo_root, 'branch', '-D', [string]$metadata.branch))
    }
    if ($exit -eq 0) {
        return
    }
    Write-Err "git worktree remove failed; falling back to safe directory removal under proxy root."
}

Remove-Item -LiteralPath $target -Recurse -Force
