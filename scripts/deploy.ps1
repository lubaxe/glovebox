<#
.SYNOPSIS
Builds glovebox for the Pi and installs it on a dev Pi over SSH.

.DESCRIPTION
Cross-compiles cmd/glovebox for linux/arm64, copies the binary, systemd unit
and (if the Pi doesn't have one yet) the example config, then restarts the
service. Needs key-based SSH and passwordless sudo on the Pi.

.EXAMPLE
./scripts/deploy.ps1
./scripts/deploy.ps1 -Target glovebox@192.168.1.50
#>
param(
    [string]$Target = $(if ($env:GLOVEBOX_TARGET) { $env:GLOVEBOX_TARGET } else { 'glovebox@192.168.1.50' })
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$version = (git describe --always --dirty 2>$null)
if (-not $version) { $version = 'dev' }
$outDir = Join-Path ([IO.Path]::GetTempPath()) 'glovebox-deploy'
New-Item -ItemType Directory -Force $outDir | Out-Null
$out = Join-Path $outDir 'glovebox'

Write-Host "Building glovebox $version for linux/arm64"
$env:GOOS = 'linux'; $env:GOARCH = 'arm64'; $env:CGO_ENABLED = '0'
try {
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o $out ./cmd/glovebox
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

Write-Host "Copying to $Target"
scp -q -o BatchMode=yes $out deploy/glovebox.service deploy/glovebox.toml "${Target}:/tmp/"
if ($LASTEXITCODE -ne 0) { throw 'scp failed' }

Write-Host 'Installing and restarting the service'
$install = @'
set -e
sudo install -m 755 /tmp/glovebox /usr/local/bin/glovebox
sudo install -m 644 /tmp/glovebox.service /etc/systemd/system/glovebox.service
sudo install -d /etc/glovebox
[ -e /etc/glovebox/glovebox.toml ] || sudo install -m 644 /tmp/glovebox.toml /etc/glovebox/glovebox.toml
rm -f /tmp/glovebox /tmp/glovebox.service /tmp/glovebox.toml
sudo systemctl daemon-reload
sudo systemctl enable -q glovebox
sudo systemctl restart glovebox
sleep 1
systemctl --no-pager --lines=5 status glovebox
'@
ssh -o BatchMode=yes $Target ($install -replace "`r", '')
if ($LASTEXITCODE -ne 0) { throw 'install failed' }
