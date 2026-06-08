# deploy.ps1 — build the server for Linux and ship it to the Hetzner box.
#
# Run from the shonei-server repo root:  .\deploy\deploy.ps1
#
# Ships the binary + traders.json only. The runtime state files
# (pricelog.json / traderstock.json) live on the server and are deliberately
# NOT uploaded, so redeploying code never wipes the live market.
#
# Prereq: one-time server bootstrap done (see deploy/README.md), and your SSH
# key is on the box so `ssh root@<ip>` connects without a password prompt.

$ErrorActionPreference = "Stop"

# ── Config ──────────────────────────────────────────────────────────
$ServerIP  = "5.161.112.237"
$RemoteDir = "/opt/shonei"
$Service   = "shonei"
$Binary    = "shonei-market-linux"

# ── Build ───────────────────────────────────────────────────────────
Write-Host "Building $Binary for linux/amd64..." -ForegroundColor Cyan
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -o $Binary .
$buildOk = $?
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
if (-not $buildOk -or $LASTEXITCODE -ne 0) { throw "go build failed" }

# ── Ship ────────────────────────────────────────────────────────────
# Stop first: scp over a running executable fails with "text file busy".
Write-Host "Stopping remote service..." -ForegroundColor Cyan
ssh root@$ServerIP "systemctl stop $Service"

Write-Host "Uploading binary + traders.json..." -ForegroundColor Cyan
scp $Binary traders.json "root@${ServerIP}:$RemoteDir/"
if ($LASTEXITCODE -ne 0) { throw "scp failed" }

# Make sure it's executable, then start and show status.
Write-Host "Starting remote service..." -ForegroundColor Cyan
ssh root@$ServerIP "chmod +x $RemoteDir/$Binary && systemctl start $Service && systemctl --no-pager status $Service"

Write-Host "Done." -ForegroundColor Green
