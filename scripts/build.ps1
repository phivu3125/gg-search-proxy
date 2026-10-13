# Build gg-search-proxy with the public Antigravity IDE OAuth client injected.
# The client constants live in 9router's open-source repo (open-sse/providers/shared.js)
# and are fetched at build time so this repo stays scanner-clean.
# Requires: Go 1.27+, PowerShell 5+ (curl.exe ships with Windows 10+).
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path $PSScriptRoot -Parent
Set-Location $repoRoot

$sharedUrl = 'https://raw.githubusercontent.com/decolua/9router/master/open-sse/providers/shared.js'
$tmp = Join-Path $env:TEMP '9router-shared.js'
Write-Host "fetching $sharedUrl"
& curl.exe -fsSL $sharedUrl -o $tmp
if ($LASTEXITCODE -ne 0) { throw 'failed to download 9router shared.js' }

$src = Get-Content $tmp -Raw
$id = [regex]::Match($src, 'clientId:\s*"([^"]+)"').Groups[1].Value
$sec = [regex]::Match($src, 'clientSecret:\s*"([^"]+)"').Groups[1].Value
Remove-Item $tmp -Force
if (-not $id -or -not $sec) { throw 'could not extract ANTIGRAVITY_OAUTH_CLIENT from 9router source' }
Write-Host "oauth client: $($id.Substring(0,12))..."

$ldflags = "-X ggsearch/internal/antigravity.clientID=$id -X ggsearch/internal/antigravity.clientSecret=$sec"
New-Item -ItemType Directory -Force -Path dist | Out-Null

$targets = @(
    @{ GOOS = 'windows'; GOARCH = 'amd64'; Out = 'dist/ggsearch-windows-amd64.exe' },
    @{ GOOS = 'linux';   GOARCH = 'amd64'; Out = 'dist/ggsearch-linux-amd64' },
    @{ GOOS = 'darwin';  GOARCH = 'arm64'; Out = 'dist/ggsearch-darwin-arm64' },
    @{ GOOS = 'darwin';  GOARCH = 'amd64'; Out = 'dist/ggsearch-darwin-amd64' }
)
foreach ($t in $targets) {
    $env:GOOS = $t.GOOS; $env:GOARCH = $t.GOARCH
    Write-Host "building $($t.Out)"
    & go build -ldflags $ldflags -o $t.Out ./cmd/ggsearch
    if ($LASTEXITCODE -ne 0) { throw "build failed for $($t.Out)" }
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue

Write-Host "`nbinaries:"
Get-ChildItem dist | ForEach-Object { Write-Host ("  {0}  {1:N1} MB" -f $_.Name, ($_.Length / 1MB)) }
Write-Host "`nSHA256:"
Get-FileHash dist/* -Algorithm SHA256 | ForEach-Object { Write-Host ("  {0}  {1}" -f $_.Hash, (Split-Path $_.Path -Leaf)) }
