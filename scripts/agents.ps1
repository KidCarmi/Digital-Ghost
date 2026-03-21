# scripts/agents.ps1 — Digital Ghost Agent Team launcher
#
# Runs three Claude Opus 4.6 agents in a feature ideation loop:
#   💡 Ideas Agent  — proposes features, refines based on feedback
#   🏗️  Architect    — reviews architecture fit and technical feasibility
#   🔒 CISO         — reviews security, privacy, and threat model
#
# Usage:
#   .\scripts\agents.ps1                              # 2 review rounds, open topic
#   .\scripts\agents.ps1 -Topic "search UX"           # focus on a specific area
#   .\scripts\agents.ps1 -Rounds 3 -Topic "performance"
#   .\scripts\agents.ps1 -Rounds 1                    # quick single-round pass
#
# Requirements:
#   $env:ANTHROPIC_API_KEY must be set before running.
#   Get a key at https://console.anthropic.com

param(
    [int]    $Rounds = 2,
    [string] $Topic  = ""
)

# ── API key check ─────────────────────────────────────────────────────────────
if (-not $env:ANTHROPIC_API_KEY) {
    Write-Host ""
    Write-Host "  ERROR: ANTHROPIC_API_KEY is not set." -ForegroundColor Red
    Write-Host ""
    Write-Host "  Set it with:"
    Write-Host '    $env:ANTHROPIC_API_KEY = "sk-ant-..."'
    Write-Host ""
    exit 1
}

# ── Build ─────────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "  Building agent team..." -ForegroundColor Cyan
go build -o bin\dg-agents.exe .\cmd\agents
if ($LASTEXITCODE -ne 0) {
    Write-Host "  Build failed." -ForegroundColor Red
    exit 1
}

# ── Run ───────────────────────────────────────────────────────────────────────
$args = @("-rounds", $Rounds)
if ($Topic -ne "") {
    $args += @("-topic", $Topic)
}

.\bin\dg-agents.exe @args
