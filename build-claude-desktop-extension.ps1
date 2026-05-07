$ErrorActionPreference = 'Stop'

$basePath = Split-Path -Parent $MyInvocation.MyCommand.Path
$distRoot = Join-Path $basePath 'dist'
$extensionRoot = Join-Path $distRoot 'claude-code-proxy-browser'
$serverRoot = Join-Path $extensionRoot 'server'
$serverBinRoot = Join-Path $serverRoot 'bin'
$exeSource = Join-Path $basePath 'bin\claude-code-proxy.exe'
$exeDest = Join-Path $serverBinRoot 'claude-code-proxy.exe'
$launcherSource = Join-Path $basePath 'start-antigravity-browser-mcp.ps1'
$launcherDest = Join-Path $serverRoot 'start-antigravity-browser-mcp.ps1'
$manifestPath = Join-Path $extensionRoot 'manifest.json'
$zipPath = Join-Path $distRoot 'claude-code-proxy-browser.zip'
$mcpbPath = Join-Path $distRoot 'claude-code-proxy-browser.mcpb'
$dxtPath = Join-Path $distRoot 'claude-code-proxy-browser.dxt'

function Assert-ChildPath {
    param(
        [Parameter(Mandatory = $true)] [string]$Parent,
        [Parameter(Mandatory = $true)] [string]$Child
    )

    $parentFull = [System.IO.Path]::GetFullPath($Parent).TrimEnd('\') + '\'
    $childFull = [System.IO.Path]::GetFullPath($Child)
    if (-not $childFull.StartsWith($parentFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to operate outside $parentFull`: $childFull"
    }
}

if (-not (Test-Path -LiteralPath $launcherSource)) {
    throw "Missing launcher script: $launcherSource"
}

if (-not (Test-Path -LiteralPath $exeSource)) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "Missing $exeSource and Go is not available in PATH."
    }
    New-Item -ItemType Directory -Path (Split-Path -Parent $exeSource) -Force | Out-Null
    & go build -o $exeSource .
    if ($LASTEXITCODE -ne 0) {
        throw 'Go build failed.'
    }
}

New-Item -ItemType Directory -Path $distRoot -Force | Out-Null
Assert-ChildPath -Parent $distRoot -Child $extensionRoot
if (Test-Path -LiteralPath $extensionRoot) {
    Remove-Item -LiteralPath $extensionRoot -Recurse -Force
}
New-Item -ItemType Directory -Path $serverBinRoot -Force | Out-Null

Copy-Item -LiteralPath $launcherSource -Destination $launcherDest -Force
Copy-Item -LiteralPath $exeSource -Destination $exeDest -Force

$manifest = @'
{
  "manifest_version": "0.3",
  "name": "claude-code-proxy-browser",
  "display_name": "Claude Code Proxy Browser",
  "version": "0.1.0",
  "description": "Local visible browser-control MCP bridge for Claude Desktop and Claude Code.",
  "long_description": "Claude Code Proxy Browser exposes local browser-control tools through MCP. It can check browser status, list pages, navigate, read visible page text, capture screenshots, inspect console/network errors, move a visible cursor overlay, click, type, press keys, and wait for page content. The server runs locally and talks to the bundled Claude Code Proxy executable.",
  "author": {
    "name": "Local Claude Code Proxy"
  },
  "license": "MIT",
  "keywords": ["browser", "mcp", "automation", "local", "claude-code-proxy"],
  "tools": [
    {
      "name": "browser_status",
      "description": "Check Chrome, Antigravity extension, and browser-control connection status."
    },
    {
      "name": "browser_pages",
      "description": "List open Chrome pages available to the browser bridge."
    },
    {
      "name": "browser_navigate",
      "description": "Open or navigate the active Chrome page to a URL."
    },
    {
      "name": "browser_snapshot",
      "description": "Read visible page text and a short list of interactive elements."
    },
    {
      "name": "browser_screenshot",
      "description": "Take a screenshot of the active Chrome page and save it to a local PNG file."
    },
    {
      "name": "browser_console",
      "description": "Check the active Chrome page for console errors, runtime exceptions, failed loads, and HTTP errors."
    },
    {
      "name": "browser_move",
      "description": "Move the visible overlay cursor to a selector, text, or coordinate without clicking."
    },
    {
      "name": "browser_click",
      "description": "Move the visible overlay cursor and click a selector, text, or coordinate."
    },
    {
      "name": "browser_type",
      "description": "Click a target, optionally clear it, and type text into the focused field."
    },
    {
      "name": "browser_press_key",
      "description": "Press a keyboard key in the active page, such as Enter, Tab, Escape, or Backspace."
    },
    {
      "name": "browser_wait",
      "description": "Wait until a selector or visible text appears on the active page."
    }
  ],
  "server": {
    "type": "binary",
    "entry_point": "server/start-antigravity-browser-mcp.ps1",
    "mcp_config": {
      "command": "pwsh",
      "args": ["-NoProfile", "-File", "${__dirname}/server/start-antigravity-browser-mcp.ps1"],
      "env": {
        "CCP_BROWSER_MCP": "antigravity-browser"
      }
    }
  },
  "compatibility": {
    "claude_desktop": ">=0.10.0",
    "platforms": ["win32"]
  }
}
'@

Set-Content -Path $manifestPath -Value $manifest -Encoding UTF8

foreach ($artifact in @($zipPath, $mcpbPath, $dxtPath)) {
    if (Test-Path -LiteralPath $artifact) {
        Remove-Item -LiteralPath $artifact -Force
    }
}

Compress-Archive -Path (Join-Path $extensionRoot '*') -DestinationPath $zipPath -Force
Copy-Item -LiteralPath $zipPath -Destination $mcpbPath -Force
Copy-Item -LiteralPath $zipPath -Destination $dxtPath -Force

[pscustomobject]@{
    unpacked_extension = $extensionRoot
    mcpb_package       = $mcpbPath
    dxt_package        = $dxtPath
    manifest           = $manifestPath
} | ConvertTo-Json -Depth 4
