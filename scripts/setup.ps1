# =============================================================================
# Digital Ghost -- Prerequisites Setup Script (Windows)
# =============================================================================
# Checks for every prerequisite, installs if missing, upgrades if outdated.
#
# Requirements:
#   Windows 10 (build 1809+) or Windows 11
#   PowerShell 5.1+ (built-in) or PowerShell 7+ (recommended)
#   Run as a normal user -- the script will use winget for installs
#
# Usage:
#   Right-click > "Run with PowerShell"
#   OR from a PowerShell terminal:
#     .\scripts\setup.ps1              # Check + install/upgrade everything
#     .\scripts\setup.ps1 -CheckOnly   # Audit only, nothing installed
#     .\scripts\setup.ps1 -NoModel     # Skip the ~4.5 GB model download
# =============================================================================

[CmdletBinding()]
param(
    [switch]$CheckOnly,
    [switch]$NoModel
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# -- Constants -----------------------------------------------------------------
$REQUIRED_GO_VERSION  = [version]"1.22"
$OLLAMA_MIN_VERSION   = [version]"0.1.30"
$DEFAULT_MODEL        = "llava:7b"
$OLLAMA_API           = "http://127.0.0.1:11434"
$SCRIPT_DIR           = Split-Path -Parent $MyInvocation.MyCommand.Path
$REPO_ROOT            = Split-Path -Parent $SCRIPT_DIR

# -- Colour helpers ------------------------------------------------------------
function Write-Info    { param($msg) Write-Host "[INFO]  $msg" -ForegroundColor Cyan }
function Write-Ok      { param($msg) Write-Host "[ OK ]  $msg" -ForegroundColor Green }
function Write-Warn    { param($msg) Write-Host "[WARN]  $msg" -ForegroundColor Yellow }
function Write-Err     { param($msg) Write-Host "[ERR ]  $msg" -ForegroundColor Red }
function Write-Section { param($msg) Write-Host "`n---  $msg  ---" -ForegroundColor White }
function Fail          { param($msg) Write-Err $msg; exit 1 }

# -- Version comparison helper -------------------------------------------------
function Get-ParsedVersion {
    param([string]$str)
    # Extract first x.y.z from a string like "go1.22.5 windows/amd64"
    if ($str -match '(\d+\.\d+(?:\.\d+)?)') {
        try { return [version]$Matches[1] } catch { return $null }
    }
    return $null
}

# -- Winget wrapper ------------------------------------------------------------
function Install-WithWinget {
    param(
        [string]$FriendlyName,
        [string]$WingetId
    )
    if ($CheckOnly) {
        Write-Warn "Would install via winget: $WingetId"
        return
    }
    Write-Info "Installing $FriendlyName via winget..."
    winget install --id $WingetId --silent --accept-source-agreements --accept-package-agreements
    Write-Ok "$FriendlyName installed"
}

function Upgrade-WithWinget {
    param([string]$FriendlyName, [string]$WingetId)
    if ($CheckOnly) {
        Write-Warn "Would upgrade via winget: $WingetId"
        return
    }
    Write-Info "Upgrading $FriendlyName via winget..."
    winget upgrade --id $WingetId --silent --accept-source-agreements --accept-package-agreements
    Write-Ok "$FriendlyName upgraded"
}

# -- Refresh PATH in current session -------------------------------------------
function Update-SessionPath {
    $machinePath = [System.Environment]::GetEnvironmentVariable("Path", "Machine")
    $userPath    = [System.Environment]::GetEnvironmentVariable("Path", "User")
    $env:PATH    = "$machinePath;$userPath"
}

# -- Prerequisite checks -------------------------------------------------------

function Test-AdminGuard {
    Write-Section "Admin Check"
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
               ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($isAdmin) {
        Write-Warn "Running as Administrator. This is allowed but not required."
        Write-Warn "DG itself must NOT run as Administrator (enforced at startup)."
    } else {
        Write-Ok "Running as standard user - correct"
    }
}

function Test-WindowsVersion {
    Write-Section "Windows Version"
    $build   = [System.Environment]::OSVersion.Version.Build
    $caption = (Get-CimInstance Win32_OperatingSystem).Caption
    if ($build -ge 1809) {
        Write-Ok "$caption (build $build) - DXGI Desktop Duplication supported"
    } else {
        Write-Warn "Windows build $build is below 1809. DXGI capture requires build 1809+."
    }
}

function Test-Winget {
    Write-Section "winget (Windows Package Manager)"
    if (Get-Command winget -ErrorAction SilentlyContinue) {
        $ver = (winget --version) -replace '[^0-9.]', ''
        Write-Ok "winget $ver"
    } else {
        Write-Warn "winget not found."
        Write-Warn "Install 'App Installer' from the Microsoft Store, or upgrade to Windows 11."
        Write-Warn "Alternatively, install prerequisites manually (see README.md)."
        if (-not $CheckOnly) {
            Write-Info "Opening Microsoft Store to App Installer page..."
            Start-Process "ms-windows-store://pdp/?ProductId=9NBLGGH4NNS1" -ErrorAction SilentlyContinue
            Write-Warn "Install 'App Installer' from the Store window, then re-run this script."
            Fail "winget required; cannot continue"
        }
    }
}

function Test-Git {
    Write-Section "git"
    if (Get-Command git -ErrorAction SilentlyContinue) {
        Write-Ok "git $(git --version)"
    } else {
        Write-Warn "git not found"
        Install-WithWinget "Git" "Git.Git"
        Update-SessionPath
    }
}

function Test-Go {
    Write-Section "Go $REQUIRED_GO_VERSION+"
    $installed = $null
    if (Get-Command go -ErrorAction SilentlyContinue) {
        $raw       = go version
        $installed = Get-ParsedVersion $raw
    }

    if ($installed -and $installed -ge $REQUIRED_GO_VERSION) {
        Write-Ok "Go $installed - satisfies >= $REQUIRED_GO_VERSION"
        return
    }

    if ($installed) {
        Write-Warn "Go $installed is below required $REQUIRED_GO_VERSION"
        Upgrade-WithWinget "Go" "GoLang.Go"
    } else {
        Write-Warn "Go not found"
        Install-WithWinget "Go" "GoLang.Go"
    }

    Update-SessionPath
    if (Get-Command go -ErrorAction SilentlyContinue) {
        Write-Ok "Go $(go version) ready"
    } else {
        Write-Warn "Go installed but not in PATH yet. Open a new terminal and re-run."
    }
}

function Test-Ollama {
    Write-Section "Ollama"
    $installed = $null
    if (Get-Command ollama -ErrorAction SilentlyContinue) {
        $raw       = ollama --version 2>$null
        $installed = Get-ParsedVersion ($raw -join " ")
    }

    if ($installed -and $installed -ge $OLLAMA_MIN_VERSION) {
        Write-Ok "ollama $installed"
        return
    }

    if ($installed) {
        Write-Warn "ollama $installed is below recommended $OLLAMA_MIN_VERSION"
        Upgrade-WithWinget "Ollama" "Ollama.Ollama"
    } else {
        Write-Warn "Ollama not found"
        Install-WithWinget "Ollama" "Ollama.Ollama"
    }

    Update-SessionPath
}

function Test-OllamaRunning {
    Write-Section "Ollama Service"
    $running = $false
    try {
        $resp    = Invoke-WebRequest -Uri "$OLLAMA_API/api/tags" -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
        $running = ($resp.StatusCode -eq 200)
    } catch { $running = $false }

    if ($running) {
        Write-Ok "Ollama is running at $OLLAMA_API"
        return
    }

    Write-Warn "Ollama is not running"
    if ($CheckOnly) {
        Write-Warn "Would start Ollama service"
        return
    }

    # Ollama on Windows installs as a background tray app.
    $ollamaExe = "$env:LOCALAPPDATA\Programs\Ollama\ollama.exe"
    if (-not (Test-Path $ollamaExe)) {
        $found = Get-Command ollama -ErrorAction SilentlyContinue
        if ($found) { $ollamaExe = $found.Source }
    }

    if ($ollamaExe -and (Test-Path $ollamaExe)) {
        Write-Info "Starting Ollama..."
        Start-Process $ollamaExe -ArgumentList "serve" -WindowStyle Hidden -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 3
        try {
            $resp = Invoke-WebRequest -Uri "$OLLAMA_API/api/tags" -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
            if ($resp.StatusCode -eq 200) { Write-Ok "Ollama started"; return }
        } catch {}
        Write-Warn "Ollama may still be starting. Wait a moment and re-run if needed."
    } else {
        Write-Warn "Could not locate ollama.exe. Start it manually: ollama serve"
    }
}

function Test-OllamaModel {
    Write-Section "Ollama Vision Model ($DEFAULT_MODEL)"
    if ($NoModel) {
        Write-Info "Skipping model check (-NoModel)"
        return
    }
    if (-not (Get-Command ollama -ErrorAction SilentlyContinue)) {
        Write-Warn "Ollama not installed - skipping model check"
        return
    }

    $running = $false
    try {
        $resp    = Invoke-WebRequest -Uri "$OLLAMA_API/api/tags" -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
        $running = ($resp.StatusCode -eq 200)
    } catch {}

    if (-not $running) {
        Write-Warn "Ollama not running - skipping model check"
        return
    }

    $modelBase  = $DEFAULT_MODEL -replace ":.*", ""
    $listOutput = ollama list 2>$null
    if ($listOutput -match $modelBase) {
        Write-Ok "$DEFAULT_MODEL is available locally"
        return
    }

    Write-Warn "$DEFAULT_MODEL not found locally"
    if ($CheckOnly) {
        Write-Warn "Would run: ollama pull $DEFAULT_MODEL  (~4.5 GB download)"
        return
    }
    Write-Info "Pulling $DEFAULT_MODEL (~4.5 GB - this will take a while)..."
    $prevPref = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    & ollama pull $DEFAULT_MODEL 2>&1 | ForEach-Object {
        # Strip ANSI/VT control sequences before printing
        ($_ -replace '\x1b\[[0-9;?]*[A-Za-z]', '') -replace '\x0d', '' |
            Where-Object { $_ -match '\S' } | ForEach-Object { Write-Host $_ }
    }
    $ErrorActionPreference = $prevPref
    Write-Ok "$DEFAULT_MODEL pulled"
}

function Test-DPAPI {
    Write-Section "Windows DPAPI (Encryption Key Storage)"
    # DPAPI is built into Windows. Just confirm the API is accessible.
    try {
        $testData = [System.Text.Encoding]::UTF8.GetBytes("dg-dpapi-test")
        $encrypted = [System.Security.Cryptography.ProtectedData]::Protect(
            $testData, $null,
            [System.Security.Cryptography.DataProtectionScope]::CurrentUser
        )
        $null = [System.Security.Cryptography.ProtectedData]::Unprotect(
            $encrypted, $null,
            [System.Security.Cryptography.DataProtectionScope]::CurrentUser
        )
        Write-Ok "Windows DPAPI available and working (encryption key storage ready)"
    } catch {
        Write-Warn "DPAPI test failed: $_"
        Write-Warn "This is unexpected on Windows 10/11. Check that your user profile is not corrupted."
    }
}

function Test-ScreenCapturePermission {
    Write-Section "Screen Capture (DXGI Desktop Duplication)"
    Write-Ok "DXGI Desktop Duplication requires no special permissions on Windows 10/11"
    Write-Info "Same API used by: Microsoft Teams, OBS Studio, Xbox Game Bar"
    Write-Info "Note: DG will NOT work over RDP with GPU acceleration disabled."
}

function Test-MingwOrMSVC {
    Write-Section "C Build Tools (for cgo)"
    $hasMSVC  = $false
    $hasMinGW = $false

    # Check for MSVC via vswhere
    $vsWhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
    if (Test-Path $vsWhere) {
        $vcPath = & $vsWhere -latest -products * `
            -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 `
            -property installationPath 2>$null
        if ($vcPath) { $hasMSVC = $true }
    }

    # Check for MinGW gcc
    if (Get-Command gcc -ErrorAction SilentlyContinue)                    { $hasMinGW = $true }
    if (Get-Command x86_64-w64-mingw32-gcc -ErrorAction SilentlyContinue) { $hasMinGW = $true }

    if ($hasMSVC) {
        Write-Ok "MSVC found via Visual Studio - cgo builds supported"
    } elseif ($hasMinGW) {
        Write-Ok "MinGW gcc found - cgo builds supported"
    } else {
        Write-Warn "No C compiler found (MSVC or MinGW)"
        Write-Warn "Some Go packages (cgo) require a C compiler."
        if (-not $CheckOnly) {
            Write-Info "Installing MSYS2 (includes MinGW-w64) via winget..."
            winget install --id MSYS2.MSYS2 --silent --accept-source-agreements --accept-package-agreements 2>$null
            Write-Info ""
            Write-Info "After MSYS2 finishes, open the MSYS2 MINGW64 terminal and run:"
            Write-Info "  pacman -S mingw-w64-x86_64-gcc"
            Write-Info "Then add C:\msys64\mingw64\bin to your PATH and re-run this script."
        } else {
            Write-Warn "Would install MSYS2 (winget id: MSYS2.MSYS2) to get MinGW gcc"
        }
    }
}

function Test-GoBuild {
    Write-Section "Go Build Verification"
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        Write-Warn "Go not available - skipping build check"
        return
    }
    if ($CheckOnly) {
        Write-Info "Would run: go build ./..."
        return
    }
    Push-Location $REPO_ROOT
    $prevPref = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        Write-Info "Running go mod tidy in $REPO_ROOT"
        $tidyOut = go mod tidy 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Warn "go mod tidy failed:"
            $tidyOut | ForEach-Object { Write-Warn "  $_" }
        } else {
            Write-Ok "go mod tidy succeeded"
        }

        Write-Info "Running go build ./... in $REPO_ROOT"
        $output = go build ./... 2>&1
        if ($LASTEXITCODE -eq 0) {
            Write-Ok "go build ./... succeeded"
        } else {
            Write-Warn "go build ./... failed:"
            $output | ForEach-Object { Write-Warn "  $_" }
            Write-Warn "This may be expected until all cgo/platform dependencies are in place."
        }
    } finally {
        $ErrorActionPreference = $prevPref
        Pop-Location
    }
}

function Write-Summary {
    Write-Section "Setup Summary"
    Write-Host ""

    $goVer = if (Get-Command go -ErrorAction SilentlyContinue) {
        Get-ParsedVersion (go version)
    } else { "NOT FOUND" }

    $olVer = if (Get-Command ollama -ErrorAction SilentlyContinue) {
        Get-ParsedVersion ((ollama --version 2>$null) -join " ")
    } else { "NOT FOUND" }

    Write-Host "  Go $REQUIRED_GO_VERSION+      -> $goVer"
    Write-Host "  Ollama           -> $olVer"
    Write-Host "  DPAPI keychain   -> built-in (Windows)"
    Write-Host "  DXGI capture     -> built-in (no permissions needed)"
    Write-Host ""

    if (-not $NoModel -and (Get-Command ollama -ErrorAction SilentlyContinue)) {
        $modelBase   = $DEFAULT_MODEL -replace ":.*", ""
        $modelStatus = try {
            $list = ollama list 2>$null
            if ($list -match $modelBase) { "pulled" } else { "NOT PULLED" }
        } catch { "unknown" }
        Write-Host "  $DEFAULT_MODEL       -> $modelStatus"
        Write-Host ""
    }

    Write-Host "  To build:"
    Write-Host "    cd `"$REPO_ROOT`""
    Write-Host "    go build -o bin\digitalghost.exe .\cmd\digitalghost"
    Write-Host ""
    Write-Host "  To run:   .\bin\digitalghost.exe"
    Write-Host "  To wipe:  .\bin\digitalghost.exe --wipe --confirm"
    Write-Host ""
}

# -- Main ----------------------------------------------------------------------
Write-Host ""
Write-Host "Digital Ghost -- Prerequisites Setup (Windows)" -ForegroundColor White
Write-Host "------------------------------------------------"
if ($CheckOnly) {
    Write-Host "Running in CHECK ONLY mode -- nothing will be installed`n" -ForegroundColor Yellow
}

Test-AdminGuard
Test-WindowsVersion
Test-Winget
Test-Git
Test-MingwOrMSVC
Test-DPAPI
Test-ScreenCapturePermission
Test-Go
Test-Ollama
Test-OllamaRunning
Test-OllamaModel
Test-GoBuild
Write-Summary
