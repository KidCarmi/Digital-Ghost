# Digital Ghost — Developer Guide

## What Is This?

Digital Ghost (DG) is a local-only, air-gapped contextual memory layer.
It captures your screen at low FPS, filters it through a privacy gate,
describes frames via a local VLM (Ollama llava:7b), and stores encrypted
descriptions in a searchable vector store — all on-device, nothing leaves
the machine.

---

## Quick Start (Windows)

```powershell
# 1. Prerequisites (first time only)
.\scripts\setup.ps1

# 2. Build + run
.\scripts\run.ps1

# 3. Open the search UI
.\scripts\search.ps1        # opens http://localhost:7327 in your browser
```

> On first launch a consent dialog appears. Click **Yes** to allow capture.
> Consent is stored in `C:\Users\<you>\.config\digitalghost\consent.json`
> and is not shown again unless the dialog text changes.

---

## Build & Run Commands

| Command | What it does |
|---------|-------------|
| `.\scripts\run.ps1` | Build + start the daemon |
| `.\scripts\run.ps1 -NoBuild` | Start without rebuilding |
| `.\scripts\search.ps1` | Open the search UI in browser |
| `go build ./...` | Build all packages (CI check) |
| `go build -o bin\digitalghost.exe .\cmd\digitalghost` | Build the binary |
| `.\bin\digitalghost.exe --wipe --confirm` | Erase all stored data |
| `.\bin\digitalghost.exe --status` | Show daemon status |

---

## Repository Structure

```
Digital-Ghost/
├── cmd/digitalghost/main.go       # Entry point — wires all components
├── internal/
│   ├── api/                       # HTTP server + embedded search UI
│   │   ├── server.go
│   │   └── ui.html
│   ├── capture/
│   │   ├── capture_windows.go     # DXGI capture (pixel stub, real Win32 metadata)
│   │   ├── capture_linux_x11.go   # X11 XShm (linux+x11 build tag)
│   │   ├── capture_unsupported.go # Stub for CI / other platforms
│   │   ├── frame.go               # Frame struct, pHash, Capturer interface
│   │   └── privacy.go             # Privacy gate — check BEFORE reading pixels
│   ├── config/config.go           # Validated config + defaults
│   ├── defaults/                  # Embedded default config files (blocklist.yaml)
│   ├── filter/
│   │   ├── blocklist.go           # Fail-closed blocklist with hot-reload
│   │   ├── classifier.go          # Fast content-class scorer (pre-VLM)
│   │   └── engagement.go          # Engagement scoring
│   ├── graph/coherence.go         # Graph coherence check
│   ├── inference/
│   │   ├── ollama.go              # Ollama HTTP client (infer + embed)
│   │   ├── queue.go               # Bounded frame queue
│   │   └── throttle.go            # Resource governor (CPU/GPU budget)
│   ├── storage/
│   │   ├── encrypt.go             # AES-256-GCM per-record encryption
│   │   ├── jsonstore.go           # Flat-file JSON vector store
│   │   ├── keychain.go            # DPAPI keychain (Windows)
│   │   ├── lancedb.go             # Store interface + MemoryNode type
│   │   └── retention.go           # Retention sweep + secure deletion
│   └── tray/
│       ├── consent.go             # Consent manager
│       ├── consent_windows.go     # Windows MessageBoxW consent dialog
│       └── consent_platform.go    # Fallback stub (!windows)
├── configs/
│   ├── blocklist.yaml             # Default privacy blocklist (embedded in binary)
│   └── default.yaml               # Default configuration values
├── docs/
│   ├── ARCHITECTURE.md
│   └── THREAT_MODEL.md
└── scripts/
    ├── setup.ps1                  # Install prerequisites (Go, Ollama, model)
    ├── run.ps1                    # Build + launch daemon
    └── search.ps1                 # Open search UI in browser
```

---

## Current Implementation Status

### Working end-to-end
- Config loading + validation
- DPAPI keychain + AES-256-GCM encryption
- Consent flow (Windows native MessageBoxW dialog)
- Blocklist engine — embedded in binary, hot-reload via fsnotify
- Perceptual hash deduplication
- Ollama inference client (real HTTP, retry, timeout)
- Embedding via Ollama `/api/embeddings`
- Resource governor / throttle
- Frame queue (bounded, drop-on-full)
- Engagement scorer + content classifier
- Graph coherence checker
- Flat-file JSON vector store (one encrypted file per memory node)
- Retention sweep + secure deletion (2am daily)
- Local web search UI at `http://localhost:7327`

### Stubbed — not yet implemented
| Component | File | Status |
|-----------|------|--------|
| DXGI pixel capture | `capture_windows.go:103` | Returns blank 1920×1080 frame |
| Window metadata (Win32) | `capture_windows.go:126` | Returns `{ProcessName:"stub"}` |

> **Impact of stubs**: Because all blank frames are pHash-identical, only the
> first frame passes deduplication and reaches Ollama. After that, DG idles
> (capture loop runs but drops all frames). The memory + search system works
> correctly once real frames start flowing.

---

## Multi-Monitor Design (not yet implemented)

The architecture specifies one `captureLoop` goroutine per display.
`Frame.DisplayIndex` (frame.go:35) is already defined.

**When implementing DXGI real capture:**
1. Enumerate adapters via `IDXGIFactory1.EnumAdapters()` → outputs via `IDXGIAdapter.EnumOutputs()`
2. Create one `IDXGIOutputDuplication` per monitor
3. Spawn one goroutine per display, each writing to the shared `frames chan<- *Frame`
4. Set `Frame.DisplayIndex` to the monitor index
5. Privacy gate stays global — if any sensitive window is open on any monitor, all capture pauses
6. `NewCapturer()` returns a `MultiDisplayCapturer` wrapping N single-display capturers

---

## Data Locations (Windows)

| Path | Contents |
|------|----------|
| `C:\Users\<you>\.config\digitalghost\` | Config + blocklist + consent record |
| `C:\Users\<you>\.local\share\digitalghost\memories\` | Encrypted memory nodes (JSON) |
| `C:\Users\<you>\.local\share\digitalghost\dg.log` | Daemon log |
| Windows Credential Manager | Encryption key (never on disk) |

---

## Configuration

Edit `~/.config/digitalghost/config.yaml` to override defaults:

```yaml
capture:
  fps: 2              # frames per second (0 < fps <= 30)
  idle_fps: 0.5       # fps when system is idle
  hash_threshold: 10  # pHash Hamming distance below which frames are duplicates

resource_budget:
  max_cpu_pct: 20     # max CPU % DG is allowed to use
  max_inference_per_min: 6

inference:
  model: llava:7b
  embed_model: nomic-embed-text  # dedicated embedding model (better search quality)
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
```

---

## Branch & Commit Conventions

- Active development branch: `claude/digital-ghost-architecture-cFmbd`
- Commit message format: `type: short description` (e.g. `fix:`, `feat:`, `add:`, `refactor:`)
- Always run `go build ./...` before committing
- Push to `origin claude/digital-ghost-architecture-cFmbd` — never to main

---

## Architecture Principles (non-negotiable)

1. **Privacy gate is first** — window metadata checked before any pixel is read
2. **Fail-closed** — if blocklist missing/corrupt, capture halts entirely
3. **Resource budget is hard** — no bypass of CPU/GPU governor
4. **Key never on disk** — encryption key lives only in OS keychain + process memory
5. **Consent required** — daemon refuses to start without valid consent record
6. **Tray icon mandatory** — no silent background mode

See `docs/ARCHITECTURE.md` and `docs/THREAT_MODEL.md` for full details.
