# Digital Ghost

**Local-only, air-gapped contextual memory for your desktop.**

Digital Ghost runs quietly in the background, watches your screen at 2 fps, filters everything through a privacy gate, describes what it sees using a local AI model, and stores encrypted notes you can search later — all without a single byte leaving your machine.

```
┌──────────────────────────────────────────────────────────────────┐
│  100% LOCAL. NO NETWORK. NO CLOUD. ALL DATA STAYS ON YOUR MACHINE│
│  Ollama runs locally. Storage is on-disk, AES-256-GCM encrypted. │
└──────────────────────────────────────────────────────────────────┘
```

---

## Quick Start (Windows)

```powershell
# 1. Prerequisites (first time only)
.\scripts\setup.ps1        # installs Go, Ollama, pulls llava:7b

# 2. Build + run
.\scripts\run.ps1

# 3. Open the search + timeline UI
.\scripts\search.ps1       # opens http://localhost:7327 in your browser
```

On first launch a native consent dialog appears. Click **Yes** to allow capture. A purple system tray icon is always visible while DG is running — right-click to **Pause**, **Open Search UI**, or **Stop**.

---

## What It Does

| Step | What happens |
|------|-------------|
| **Capture** | Screenshots the primary monitor at 2 fps using GDI BitBlt (or DXGI if configured) |
| **Privacy gate** | Checks foreground window app name, title, and browser URL against a blocklist *before* any pixel is read. Fail-closed. |
| **Dedup** | Perceptual hash (pHash) skips frames that haven't changed |
| **Describe** | Novel frames are sent to `llava:7b` via Ollama; gets back a natural-language description + tags |
| **Score** | Engagement score (dwell time × input activity × content class) filters out noise |
| **Store** | High-value frames are AES-256-GCM encrypted and written to disk; key stays in Windows Credential Manager |
| **Search** | Ask questions at `http://localhost:7327` — semantic search powered by `nomic-embed-text` embeddings |

---

## Current Status

### Working end-to-end

- **Screen capture** — GDI BitBlt on Windows (primary monitor); DXGI Desktop Duplication available via `capture.backend: dxgi`
- **Window metadata** — Win32 `GetForegroundWindow` + `QueryFullProcessImageNameW` (real process name + window title)
- **Browser URL extraction** — Chromium family via `EnumChildWindows` / `WM_GETTEXT`; Firefox and Edge via IUIAutomation `urlbar-input` / `addressEditBox`
- **Privacy gate** — fail-closed blocklist (19 apps, 63 URL patterns, 22 title patterns); pause/resume from tray
- **Perceptual hash deduplication** — frames within Hamming distance 10 are skipped
- **Ollama inference** — real HTTP to `llava:7b` with retry and timeout; warm, uncertainty-aware prompt (won't fabricate content it can't see)
- **Embeddings** — `nomic-embed-text` via Ollama `/api/embeddings`
- **Resource governor** — configurable CPU/GPU budget; yields to foreground workloads
- **Encrypted storage** — flat-file JSON store (`~/.local/share/digitalghost/memories/`), one AES-256-GCM file per node; key in Windows Credential Manager
- **Vector search** — in-memory cosine similarity over stored embeddings (works well up to ~10 k nodes)
- **Retention sweep** — daily at 2 am; secure deletion (3-pass overwrite)
- **Web UI** at `http://localhost:7327`:
  - **Search tab** — semantic + LLM-synthesised chat answers with source cards
  - **Timeline tab** — all memories newest-first, grouped by day, paginated (50/page)
  - **Manage panel** — delete today / this week / this month / all (CSRF-protected)
- **System tray** — purple circle icon; Pause Capture / Resume Capture / Open Search UI / Stop
- **Consent flow** — native `MessageBoxW` dialog on first run; record stored in `~/.config/digitalghost/consent.json`

### Not yet implemented

| Feature | Notes |
|---------|-------|
| Multi-monitor capture | Architecture designed; `Frame.DisplayIndex` exists. Needs one goroutine per `IDXGIOutput`. |
| Sensitive-input masking | Detect focused `<input type="password">` via UIA `UIA_IsPasswordPropertyId`; black-out region before sending to Ollama |
| Linux / macOS | X11 (`XShmGetImage`) and ScreenCaptureKit stubs exist; not wired |

---

## Architecture

```
Desktop Session
│
│  Win32 APIs (GetForegroundWindow, QueryFullProcessImageNameW)
│  IUIAutomation (browser URL — Firefox, Edge, Chrome)
│                                     │
│                            ┌────────▼────────┐
│                            │  Privacy Gate   │  ← checks BEFORE pixels
│                            │  (blocklist +   │
│                            │   pause state)  │
│                            └────────┬────────┘
│                                PASS │ (BLOCKED → skip frame)
│  GDI BitBlt / DXGI Duplication     │
│  ──────────────────────────────────▼
│                            ┌────────────────┐
│                            │  pHash dedup   │  ← skip identical frames
│                            └────────┬───────┘
│                                     │
│                            ┌────────▼───────┐
│                            │  Frame Queue   │  ← bounded, drop-on-full
│                            │  (100 frames)  │
│                            └────────┬───────┘
│                                     │
│                       ┌─────────────▼──────────────┐
│                       │     Resource Governor       │  ← CPU/GPU budget
│                       │  Yields under load          │
│                       └─────────────┬──────────────┘
│                                     │
│                       ┌─────────────▼──────────────┐
│                       │   Ollama llava:7b           │  ← local HTTP only
│                       │   description + tags        │
│                       └─────────────┬──────────────┘
│                                     │
│                       ┌─────────────▼──────────────┐
│                       │  Engagement scorer +        │
│                       │  Graph coherence filter     │
│                       └─────────────┬──────────────┘
│                                     │
│                       ┌─────────────▼──────────────┐
│                       │  AES-256-GCM encrypted      │
│                       │  flat-file JSON store       │  ← key in OS keychain
│                       └────────────────────────────┘
│
└── HTTP :7327  Search tab · Timeline tab · Manage panel
```

---

## Repository Structure

```
Digital-Ghost/
├── cmd/digitalghost/main.go       # Entry point — wires all components
├── internal/
│   ├── api/
│   │   ├── server.go              # HTTP server (/api/query, /api/timeline, /api/chat, /api/delete)
│   │   └── ui.html                # Embedded single-page UI (Search + Timeline tabs)
│   ├── capture/
│   │   ├── privacy.go             # ★ Privacy gate — first thing checked, last thing changed
│   │   ├── frame.go               # Frame struct, pHash, Capturer interface
│   │   ├── capture_windows.go     # GDI BitBlt capture + Win32 window metadata
│   │   ├── dxgi_windows.go        # DXGI Desktop Duplication backend (opt-in)
│   │   ├── uia_windows.go         # IUIAutomation browser URL extraction (Firefox + Edge)
│   │   ├── capture_linux_x11.go   # X11 XShm stub
│   │   └── capture_unsupported.go # CI / other platforms stub
│   ├── config/config.go           # Validated config + safe defaults
│   ├── defaults/                  # Embedded default blocklist (in binary)
│   ├── filter/
│   │   ├── blocklist.go           # Fail-closed blocklist with fsnotify hot-reload
│   │   ├── classifier.go          # Fast content-class scorer (pre-VLM)
│   │   └── engagement.go          # Engagement scoring (dwell × input × content class)
│   ├── graph/coherence.go         # Graph coherence check (isolated node penalty)
│   ├── inference/
│   │   ├── ollama.go              # Ollama HTTP client — infer + embed + generate
│   │   ├── queue.go               # Bounded frame queue
│   │   └── throttle.go            # Resource governor
│   ├── storage/
│   │   ├── encrypt.go             # AES-256-GCM per-record encryption
│   │   ├── jsonstore.go           # Flat-file JSON vector store (lanceDBConn impl)
│   │   ├── keychain.go            # OS keychain — DPAPI on Windows
│   │   ├── lancedb.go             # Store interface, MemoryNode, TimelineEntry types
│   │   └── retention.go           # Daily retention sweep + secure deletion
│   └── tray/
│       ├── consent.go             # Consent manager
│       ├── consent_windows.go     # Windows: MessageBoxW consent + systray icon
│       └── consent_platform.go    # Non-Windows stub
├── configs/
│   ├── blocklist.yaml             # Default privacy blocklist (embedded in binary)
│   └── default.yaml               # Default configuration values
├── docs/
│   ├── ARCHITECTURE.md
│   └── THREAT_MODEL.md
└── scripts/
    ├── setup.ps1                  # Install Go, Ollama, pull llava:7b
    ├── run.ps1                    # Build + launch daemon
    └── search.ps1                 # Open http://localhost:7327 in browser
```

---

## Configuration

Edit `~/.config/digitalghost/config.yaml` to override defaults:

```yaml
capture:
  fps: 2                    # frames per second (max 30)
  hash_threshold: 10        # pHash Hamming distance for duplicate detection
  backend: gdi              # "gdi" (default) or "dxgi" (GPU-accelerated)

resource_budget:
  max_cpu_pct: 20
  max_inference_per_min: 6

inference:
  model: llava:7b
  embed_model: nomic-embed-text  # dedicated embedding model (better search quality)
  timeout_sec: 120          # llava:7b cold-start can take 60-90s

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

The blocklist hot-reloads within 500ms of any file change — no restart needed.

---

## Data Locations (Windows)

| Path | Contents |
|------|----------|
| `%USERPROFILE%\.config\digitalghost\` | `config.yaml`, `blocklist.yaml`, `consent.json` |
| `%USERPROFILE%\.local\share\digitalghost\memories\` | Encrypted memory nodes (`.enc` files + `index.json`) |
| Windows Credential Manager | Encryption key (never written to disk) |

---

## CLI Reference

```powershell
digitalghost.exe                       # Start the daemon (normal usage)
digitalghost.exe --status              # Show config summary and exit
digitalghost.exe --wipe --confirm      # Permanently delete all stored memories
digitalghost.exe --config <path>       # Use a custom config file
```

---

## Security Properties

- **Local only** — the only outbound connection is `127.0.0.1:11434` (Ollama). Verifiable with `netstat -b`.
- **Encrypted at rest** — every memory node is AES-256-GCM encrypted before being written to disk. The key lives in Windows Credential Manager and is never written to a file.
- **Privacy-gate-first** — window metadata (app name, title, URL) is checked *before* any pixel buffer is allocated. If the gate cannot make a determination, it blocks.
- **Fail-closed blocklist** — if `blocklist.yaml` is missing or corrupt, capture halts entirely. It never fails open.
- **CSRF protection** — the delete endpoint requires a `X-DG-CSRF-Token` header set at server startup. Cross-origin pages cannot forge it.
- **No root** — the daemon refuses to start as Administrator/root.
- **Tray icon required** — there is no silent/headless mode. If the tray icon cannot be shown, the daemon exits.

See [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the full STRIDE analysis.

---

## License

MIT
