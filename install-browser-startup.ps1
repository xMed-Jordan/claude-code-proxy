$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $basePath
$exe = Join-Path $basePath 'bin\connect-ai-proxy.exe'

function Invoke-ConnectProxy {
    param(
        [Parameter(ValueFromRemainingArguments = $true)]
        [string[]]$ProxyArgs
    )

    if (Test-Path -LiteralPath $exe) {
        & $exe @ProxyArgs
    } else {
        & go run . @ProxyArgs
    }
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
}

Invoke-ConnectProxy uninstall-startup
Invoke-ConnectProxy sync browser-apply
Write-Host 'Installed browser-only Claude configuration. No local proxy startup task was created.'
