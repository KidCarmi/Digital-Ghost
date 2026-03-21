# =============================================================================
# Digital Ghost -- Build and Run Script
# =============================================================================
# Usage:
#   .\scripts\run.ps1          # Build and run
#   .\scripts\run.ps1 -NoBuild # Skip build, just run the existing binary
# =============================================================================

param([switch]$NoBuild)

$ROOT = Split-Path -Parent $PSScriptRoot
$BIN  = "$ROOT\bin\digitalghost.exe"

Set-Location $ROOT

if (-not $NoBuild) {
    Write-Host "Building..." -ForegroundColor Cyan
    go build -o bin\digitalghost.exe .\cmd\digitalghost
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Build failed." -ForegroundColor Red
        exit 1
    }
    Write-Host "Build OK." -ForegroundColor Green
}

Write-Host "Starting Digital Ghost..." -ForegroundColor Cyan
& $BIN
