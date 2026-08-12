# deploy-windows.ps1 — Install SAND Vault on Windows as a background service.
#
# Usage (run in an elevated PowerShell window):
#   .\scripts\deploy-windows.ps1 [-Binary <path>] [-Port <int>] [-Bind <ip>]
#
# Defaults:
#   Binary  = .\sand.exe
#   Port    = 8080
#   Bind    = 127.0.0.1
#
# Requires: NSSM (Non-Sucking Service Manager) installed and on PATH.
#   winget install nssm  — or download from https://nssm.cc
#
# After running:
#   Get-Service sand
#   nssm status sand

param(
    [string]$Binary = "",
    [int]$Port      = 8080,
    [string]$Bind   = "127.0.0.1"
)

$ErrorActionPreference = "Stop"

# ── Locate binary ─────────────────────────────────────────────────────────────
if (-not $Binary) {
    foreach ($candidate in @(".\sand.exe", ".\dist\sand-*-windows-amd64.exe")) {
        $match = Get-Item $candidate -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($match) { $Binary = $match.FullName; break }
    }
}

if (-not $Binary -or -not (Test-Path $Binary)) {
    Write-Error "sand.exe not found. Build it first: make build  (or run build-release.sh)"
    exit 1
}

$BinaryFull = (Resolve-Path $Binary).Path
Write-Host "==> Deploying SAND Vault"
Write-Host "    binary : $BinaryFull"
Write-Host "    port   : $Port"
Write-Host "    bind   : $Bind"

# ── Check for NSSM ───────────────────────────────────────────────────────────
if (-not (Get-Command nssm -ErrorAction SilentlyContinue)) {
    Write-Error @"
NSSM not found. Install it first:
  winget install nssm
  # or download from https://nssm.cc and add to PATH
"@
    exit 1
}

$ServiceName = "sand"

# ── Remove old service if present ────────────────────────────────────────────
$existing = Get-Service $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    Write-Host "    removing existing service '$ServiceName'…"
    nssm stop $ServiceName confirm | Out-Null
    nssm remove $ServiceName confirm | Out-Null
}

# ── Install service ───────────────────────────────────────────────────────────
nssm install $ServiceName $BinaryFull
nssm set    $ServiceName AppParameters "serve --port $Port --bind $Bind"
nssm set    $ServiceName DisplayName   "SAND Vault"
nssm set    $ServiceName Description   "SAND Vault — split, encrypt and scatter files across cloud accounts"
nssm set    $ServiceName Start         SERVICE_AUTO_START
nssm set    $ServiceName AppStdout     "$env:ProgramData\sand\sand.log"
nssm set    $ServiceName AppStderr     "$env:ProgramData\sand\sand-error.log"
nssm set    $ServiceName AppRotateFiles 1

New-Item -ItemType Directory -Force -Path "$env:ProgramData\sand" | Out-Null

# ── Start service ─────────────────────────────────────────────────────────────
nssm start $ServiceName

Write-Host ""
Write-Host "==> SAND Vault is running!"
Write-Host "    status : Get-Service sand"
Write-Host "    logs   : $env:ProgramData\sand\sand.log"
Write-Host "    url    : http://${Bind}:${Port}"

if ($Bind -eq "127.0.0.1") {
    Write-Host ""
    Write-Host "    NOTE: bound to localhost only."
    Write-Host "    To expose publicly re-run with -Bind 0.0.0.0"
}
