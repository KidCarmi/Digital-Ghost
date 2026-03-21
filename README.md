# Digital Ghost

A local-only, air-gapped contextual memory layer for your computer.

Digital Ghost (DG) captures your screen at low FPS, runs every frame through a privacy gate, describes approved frames with a local vision-language model (Ollama llava:7b), and stores encrypted descriptions in a searchable vector store — all on-device. Nothing leaves the machine.

---

## How It Works

```
Screen (DXGI/X11)
       │
       ▼
Window Metadata          ← queried BEFORE any pixel is read
(app name, title, URL,
 PID, focused input role)
       │
       ▼
Privacy Gate             ← fail-closed: block by default
(blocklist.yaml)
       │ PASS only
       ▼
Perceptual Hash Filter   ← drops duplicate/near-duplicate frames
       │
       ▼
Bounded Frame Queue      ← 100-frame capacity, drop-oldest on overflow
       │
       ▼
Resource Governor        ← waits if CPU > 40% or GPU > 30%
       │
       ▼
Ollama VLM (llava:7b)    ← local inference only, 127.0.0.1:11434
       │
       ▼
Engagement + Coherence   ← drops low-value frames (YouTube thumbnails etc.)
       │
       ▼
Encrypted Vector Store   ← AES-256-GCM per record, key in OS keychain
       │
       ▼
Search UI (localhost:7327)
```

**The privacy gate runs before any pixel is read.** If a window is blocked, no pixel buffer is ever allocated.

---

## Quick Start (Windows)

### Prerequisites (first time only)

```powershell
.\scripts\setup.ps1
```

This installs Go, Ollama, and pulls the `llava:7b` and `nomic-embed-text` models.

### Run

```powershell
.\scripts\run.ps1
```

On first launch a consent dialog appears. Click **Yes** to allow capture. The consent record is stored at `C:\Users\<you>\.config\digitalghost\consent.json` and is not shown again unless the dialog text changes.

### Search

```powershell
.\scripts\search.ps1    # opens http://localhost:7327
```

---

## Commands

| Command | Description |
|---------|-------------|
| `.\scripts\run.ps1` | Build and start the daemon |
| `.\scripts\run.ps1 -NoBuild` | Start without rebuilding |
| `.\scripts\search.ps1` | Open the search UI in browser |
| `go build ./...` | Build all packages |
| `.\bin\digitalghost.exe --status` | Show daemon status and live store stats |
| `.\bin\digitalghost.exe --wipe --confirm` | Securely erase all stored data |

`--status` output example:

```
Digital Ghost v0.1.0
Data directory : C:\Users\you\.local\share\digitalghost
VLM model      : llava:7b
Embed model    : nomic-embed-text
Capture FPS    : 2.0 (idle: 0.5 after 30s)
Retention      : 90 days
Max CPU        : 20%  Max GPU: 15%
Memory nodes   : 1,247
Last capture   : 2026-03-21 15:42:11 (3m12s ago)
```

---

## Privacy Guarantees

### What DG never does

- Sends data off-device (no network calls outside `127.0.0.1`)
- Writes raw pixels to disk
- Stores plaintext descriptions (AES-256-GCM per record)
- Keeps the encryption key on disk (lives in OS keychain only)
- Runs without user consent (daemon exits without a valid consent record)
- Runs without a visible tray icon (no silent background mode)

### Three-layer privacy defence

**Layer 1 — Window metadata gate (zero cost, runs before any pixel read)**

Before capturing a frame, DG checks:
- Active window process name
- Window title (regex match against blocklist)
- Browser URL bar (via UI Automation — works for Chrome, Edge, Firefox)
- Focused input role (via UI Automation — blocks password fields in any app)

If any check triggers, the frame is discarded. No pixel is ever read.

**Layer 2 — Password field masking**

If the focused UI element has `UIA_IsPasswordPropertyId = true` (password inputs in any application, not just browsers), the entire frame is blocked at the gate before pixel read. This covers terminal-based SSH password prompts, KeePass, and any other app that correctly sets the UIA property.

**Layer 3 — Blocklist engine (fail-closed)**

- App name exact match
- Window title regex match
- URL pattern regex match
- Hot-reload via `fsnotify` — changes take effect within 500ms without restart
- **If `blocklist.yaml` cannot be read or parsed, ALL frames are blocked**
- The hardcoded default blocklist is embedded in the binary as a fallback

### What DG logs

Every capture decision is logged to `dg.log`:

```
BLOCKED: app=1password reason=password_manager_app_match
BLOCKED: url=https://mybank.com/login reason=url_pattern:login
PASS: app=code title="README.md — Digital-Ghost" engagement=0.87
DROPPED: reason=duplicate pHash=... hamming=3
STORED: node_id=abc123 score=0.87
```

Logs contain only metadata. No screen content is ever logged.

### If you think something was captured

```powershell
# Check what was stored
.\bin\digitalghost.exe --status

# Search your own store via the UI
.\scripts\search.ps1

# Wipe everything
.\bin\digitalghost.exe --wipe --confirm
```

Expected time from "I think something was captured" to "data is gone": **< 5 minutes**.

---

## Configuration

Edit `~/.config/digitalghost/config.yaml` to override defaults:

```yaml
capture:
  fps: 2              # frames per second (0 < fps <= 30)
  idle_fps: 0.5       # fps when system is idle
  hash_threshold: 10  # pHash Hamming distance for duplicate detection

resource_budget:
  max_cpu_pct: 20     # DG waits when system CPU exceeds this
  max_gpu_pct: 30     # DG waits when GPU utilization exceeds this
  max_inference_per_min: 6

inference:
  model: llava:7b
  embed_model: nomic-embed-text
  timeout_sec: 120    # llava:7b cold-start can take 60-90s

storage:
  retention_days: 90
  secure_delete: true
```

Add custom blocklist entries to `~/.config/digitalghost/blocklist.yaml`:

```yaml
apps:
  - "MySecretApp"
url_patterns:
  - "(?i)myinternalwiki\\.company\\.com"
title_patterns:
  - "(?i)confidential"
```

---

## Data Locations (Windows)

| Path | Contents |
|------|----------|
| `C:\Users\<you>\.config\digitalghost\` | Config, blocklist, consent record |
| `C:\Users\<you>\.local\share\digitalghost\memories\` | Encrypted memory nodes (JSON) |
| `C:\Users\<you>\.local\share\digitalghost\dg.log` | Capture decision log |
| Windows Credential Manager | Encryption key (never on disk) |

---

## Resource Usage

DG is designed to be invisible when the system is under load.

| Condition | DG behaviour |
|-----------|--------------|
| CPU > 20% | Inference paused; capture continues |
| GPU > 30% | Inference paused; capture continues |
| Rate > 6 VLM calls/min | Token bucket throttles inference |
| Frame queue full | Oldest frames dropped (logged as WARN) |
| System idle | FPS reduced to `idle_fps` (default 0.5) |

GPU utilization is read from sysfs (Intel/AMD) or `nvidia-smi` (NVIDIA). If no GPU metrics are available, DG assumes GPU is idle and proceeds.

**Worst-case impact**: ~3 minutes of GPU inference time per hour at default settings (6 calls/min × up to 30s each). In practice much less — most frames are deduped before reaching the VLM.

---

## Platform Status

| Platform | Capture | Window Metadata | Tray / Consent | Status |
|----------|---------|-----------------|----------------|--------|
| Windows 10+ | DXGI `IDXGIOutputDuplication` | Win32 + UI Automation | Native MessageBoxW | **Production-ready** |
| Linux X11 | XShm stub | Not implemented | Not implemented | In progress |
| Linux Wayland | Not implemented | Not implemented | Not implemented | Planned |
| macOS 12.3+ | Not implemented | Not implemented | Not implemented | Planned |

Multi-monitor capture is fully implemented on Windows. Each display runs an independent capture goroutine. The privacy gate is global — a blocked window on any monitor pauses capture on all monitors.

---

## System Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| OS | Windows 10 | Windows 11 |
| RAM | 8 GB | 16 GB |
| VRAM | 4 GB (llava:7b) | 8 GB |
| CPU | 4 cores | 8+ cores |
| Disk | 10 GB free | 50 GB free |
| Ollama | v0.1.0+ | Latest |

---

## Architecture

The daemon is a single Go binary. Five goroutines run concurrently:

```
main
 ├── captureLoop × N displays   — privacy gate, pHash dedup, queue write
 ├── inferenceLoop              — resource governor, Ollama, engagement score
 ├── retentionLoop              — daily sweep at 2 AM, secure deletion
 ├── metricsLoop                — CPU/GPU polling every 2s (atomic)
 └── apiServer                  — HTTP search UI at localhost:7327
```

All inter-goroutine communication is via buffered channels. No shared mutable state except the governor's CPU/GPU counters (protected by `sync/atomic`).

Both the capture and inference goroutines have `recover()` — a panic logs the error and triggers a clean shutdown rather than silently killing the process.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the full design, including how the four critical failure modes (resource contention, privacy paradox, OS permissions, semantic pollution) are addressed.

See [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md) for the STRIDE analysis, attack scenarios, and data residency details.

---

## Security Properties

| Property | Implementation |
|----------|----------------|
| Encryption | AES-256-GCM, random 96-bit IV per record |
| Key storage | Windows Credential Manager (DPAPI) — never on disk |
| Integrity | HMAC-SHA256 per node — tampered nodes quarantined |
| Network | Zero outbound connections. All Ollama calls to `127.0.0.1` |
| Privilege | Exits immediately if run as root or with elevated privileges |
| Consent | Required on every startup. Timestamped + dialog-text-hashed record |

Verify no outbound network connections:

```powershell
netstat -b | findstr digitalghost
```

---

## Repository Layout

```
cmd/digitalghost/main.go        Entry point
internal/
  api/                          HTTP server + embedded search UI
  capture/
    capture_windows.go          DXGI capture, Win32 metadata, multi-monitor loops
    uia_windows.go              UI Automation — password field detection, browser URLs
    capture_linux_x11.go        X11 (partial)
    frame.go                    Frame struct, pHash, Capturer interface
    privacy.go                  Privacy gate — metadata checks before pixel read
  config/config.go              Validated config + defaults
  filter/
    blocklist.go                Fail-closed blocklist with hot-reload
    classifier.go               Fast content-class scorer (pre-VLM, < 1ms)
    engagement.go               Engagement scoring
  graph/coherence.go            Graph coherence check
  inference/
    ollama.go                   Ollama HTTP client (retry, timeout)
    queue.go                    Bounded frame queue
    throttle.go                 Resource governor (CPU/GPU/rate-limit)
  storage/
    encrypt.go                  AES-256-GCM per-record encryption
    jsonstore.go                Flat-file JSON vector store
    keychain.go                 DPAPI keychain (Windows)
    lancedb.go                  Store interface + MemoryNode type
    retention.go                Retention sweep + secure deletion
  tray/
    consent.go                  Consent manager
    consent_windows.go          Windows native consent dialog
docs/
  ARCHITECTURE.md               Full technical design
  THREAT_MODEL.md               STRIDE analysis
configs/
  blocklist.yaml                Default privacy blocklist (embedded in binary)
  default.yaml                  Default configuration values
scripts/
  setup.ps1                     Install prerequisites
  run.ps1                       Build + launch daemon
  search.ps1                    Open search UI in browser
```
