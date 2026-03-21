# Digital Ghost — Technical Architecture

> **Audience**: Engineers and security reviewers evaluating whether to build or trust this system.
> This document is a technical debate, not a marketing spec.

---

## 1. The Verdict First

Digital Ghost is **technically viable** under the following non-negotiable conditions:

1. The privacy gate is fail-closed and built before any inference pipeline.
2. The resource governor enforces a hard CPU/GPU budget with no bypass path.
3. Capture uses legitimate OS APIs (not exploits or accessibility hacks).
4. A permanently-visible consent indicator exists for every capture session.

If any one of these conditions is removed, the project becomes a liability. The architecture below is designed to make violating these conditions structurally impossible, not merely discouraged.

---

## 2. System Architecture

### 2.1 Data Flow

```
                     ┌────────────────────────────────────┐
                     │        OS Screen APIs              │
                     │ X11 XShm | Wayland Portal | DXGI   │
                     └─────────────────┬──────────────────┘
                                       │ raw pixels
                     ┌─────────────────▼──────────────────┐
                     │       Window Metadata              │
                     │   (app name, title, URL, PID)      │
                     │   queried BEFORE any pixel read    │
                     └─────────────────┬──────────────────┘
                                       │ metadata
                     ┌─────────────────▼──────────────────┐
                     │         Privacy Gate               │◄── blocklist.yaml
                     │   internal/capture/privacy.go      │    (fail-closed)
                     │                                    │
                     │   BLOCK ──────────────────────►X   │
                     │   PASS  ──────────────────────────►│
                     └─────────────────┬──────────────────┘
                                       │ approved frame
                     ┌─────────────────▼──────────────────┐
                     │      Perceptual Hash Filter        │
                     │   internal/capture/frame.go        │
                     │                                    │
                     │   Hamming(pHash, prev) < 10 ──►X   │
                     │   Novel frame ───────────────────►│
                     └─────────────────┬──────────────────┘
                                       │ novel frame
                     ┌─────────────────▼──────────────────┐
                     │       Bounded Frame Queue          │
                     │   internal/inference/queue.go      │
                     │   capacity: 100 frames             │
                     │   drop policy: oldest on overflow  │
                     └─────────────────┬──────────────────┘
                                       │ dequeued when idle
                     ┌─────────────────▼──────────────────┐
                     │       Resource Governor            │
                     │   internal/inference/throttle.go   │
                     │                                    │
                     │   system CPU > 40% ──► sleep 5s    │
                     │   system GPU > 30% ──► sleep 5s    │
                     │   rate > 6/min     ──► sleep Δt    │
                     └─────────────────┬──────────────────┘
                                       │ budget available
                     ┌─────────────────▼──────────────────┐
                     │     Ollama VLM Inference           │
                     │   internal/inference/ollama.go     │
                     │   POST http://127.0.0.1:11434      │
                     │   model: llava:7b                  │
                     │   timeout: 30s                     │
                     └─────────────────┬──────────────────┘
                                       │ text description + tags
                     ┌─────────────────▼──────────────────┐
                     │      Engagement Scorer             │
                     │   internal/filter/engagement.go    │
                     │                                    │
                     │   score < 0.3 ──────────────────►X │
                     └─────────────────┬──────────────────┘
                                       │ high-value description
                     ┌─────────────────▼──────────────────┐
                     │      Graph Coherence Check         │
                     │   internal/graph/coherence.go      │
                     │                                    │
                     │   isolated node, score < 0.8 ──►X  │
                     └─────────────────┬──────────────────┘
                                       │ coherent memory node
                     ┌─────────────────▼──────────────────┐
                     │   Encrypted Vector Store           │
                     │   internal/storage/lancedb.go      │
                     │   AES-256-GCM per record           │
                     │   key in OS keychain               │
                     └────────────────────────────────────┘
```

### 2.2 Goroutine Model

```
main goroutine
  │
  ├── captureLoop goroutine        (1x per display)
  │     Captures at configured FPS.
  │     Runs privacy gate and pHash dedup.
  │     Writes to FrameQueue (non-blocking; drops on full).
  │
  ├── inferenceLoop goroutine      (1x)
  │     Reads from FrameQueue.
  │     Checks resource budget before each dequeue.
  │     Calls Ollama, scores output, writes to StorageQueue.
  │
  ├── storageLoop goroutine        (1x)
  │     Reads from StorageQueue.
  │     Encrypts and writes to LanceDB.
  │     Enforces retention policy.
  │
  └── metricsLoop goroutine        (1x)
        Polls CPU/GPU utilization every 2s.
        Updates governor's shared atomic state.
```

All inter-goroutine communication is via buffered channels. No shared mutable state except the governor's CPU/GPU metrics (protected by `sync/atomic`).

---

## 3. Addressing the 4 Critical Fail Points

### 3.1 Resource Contention

**The problem**: LLaVA-13B requires ~8 GB VRAM and spikes CPU to 200% during inference. On a 16 GB laptop, this leaves the OS starved.

**The solution** (`internal/inference/throttle.go`):

```
Priority order:
  1. User's foreground work   ← always wins
  2. DG inference             ← runs in the gaps
  3. DG capture               ← always runs, but at reduced FPS under load
```

**Mechanisms**:

- **Idle detection**: A `metricsLoop` goroutine reads `/proc/stat` (Linux) and `/sys/class/drm/*/gt/gt0/rps_cur_freq_mhz` or `nvidia-smi` for GPU. When CPU load average > threshold or GPU utilization > threshold, the governor blocks the inference goroutine with a 5-second sleep and re-checks.
- **Hard rate limit**: A token bucket limits inference to `max_inference_per_min` (default: 6). This caps worst-case CPU/GPU time at ~3 minutes of inference per hour.
- **Tiered processing**: `frame.go` computes a perceptual hash in ~0.5ms. Duplicate frames (Hamming distance < 10) are dropped before they reach Ollama. In practice, 80%+ of captured frames are near-duplicates and never hit the GPU.
- **Configurable budget**: All thresholds are in `configs/default.yaml` and can be tuned per machine. Users with a dedicated AI workstation can raise limits; laptop users can lower them.
- **Model selection**: llava:7b requires ~4 GB VRAM vs. llava:13b's ~8 GB. Default is 7B. Users can override to a smaller model (e.g., moondream2) for lower resource use.

**What this cannot do**: DG cannot guarantee zero impact on GPU-intensive games or ML training. Users engaged in those activities should configure DG to auto-pause when specific apps (e.g., game launchers, `python train.py`) are in the foreground.

### 3.2 The Privacy Paradox

**The problem**: The capture pipeline will inevitably frame a banking portal, a password manager unlock screen, or a private medical record. A single capture of this data into the vector DB creates a queryable credential store.

**The solution** (`internal/capture/privacy.go` + `internal/filter/blocklist.go`):

**Defense-in-depth with three layers**:

**Layer 1 — Window Metadata Gate (zero-cost, runs before pixel read)**

Before any frame is captured, the privacy gate queries:
- Active window process name (via `/proc/<pid>/comm` on Linux, `GetWindowModuleFileName` on Windows, `NSRunningApplication` on macOS)
- Window title
- Browser URL bar (via accessibility API: AT-SPI2 on Linux, AXUIElement on macOS, UI Automation on Windows)

If any match is found in the blocklist, the frame is never read. The pixel buffer is never allocated.

**Layer 2 — Input Type Detection**

If the accessibility API reports that the focused element is an `<input type="password">` or has ARIA role `spinbutton` (common for OTP fields), the entire window is masked with a black rectangle before the frame enters the pipeline. The frame reaches the queue with the sensitive region zeroed.

**Layer 3 — Blocklist Engine** (fail-closed):

The blocklist (`internal/filter/blocklist.go`) implements:
- App name exact match
- Window title regex match
- URL regex match (for browsers)
- Hot-reload via `fsnotify` (changes take effect within 500ms without restart)
- **Fail-closed**: If `blocklist.yaml` fails to parse, the engine returns `BLOCK` for all frames. If the file is missing, the engine loads hardcoded defaults and logs a warning.
- No "allowlist override" path. A blocked window cannot be un-blocked by any runtime API.

**What metadata-first blocking covers**:
- Password managers: always have a known process name (`1password`, `keepassxc`, etc.)
- Banking: detectable via URL when using a browser
- System auth dialogs: detectable via process name (`pinentry`, `polkit`, `consent.exe`)

**What it cannot cover**:
- A custom internal tool with no known process name that happens to display sensitive data
- A terminal displaying a secret that was pasted (no URL, no known process name beyond `gnome-terminal`)

**Mitigation for the terminal case**: Terminal windows are captured, but DG applies a post-capture regex scan for common secret patterns (AWS keys, private key headers, JWT tokens) and discards frames that match before they reach Ollama.

### 3.3 OS Level Permissions

**The problem**: Modern OSes treat arbitrary screen scraping as a threat. Wayland broke X11 screen capture intentionally. macOS TCC requires user approval. A binary that needs special permissions looks like malware.

**The solution**: Use only legitimate, documented, user-consent-gated APIs.

| OS | API | Permission Model | Same APIs Used By |
|----|-----|-----------------|-------------------|
| Linux X11 | `XShmGetImage` | No special permission required for own X session | OBS, scrot, xwd, ffmpeg |
| Linux Wayland | `xdg-desktop-portal` ScreenCast + PipeWire | One-time OS confirmation dialog | GNOME Screen Recorder, OBS |
| macOS 12.3+ | `ScreenCaptureKit` | TCC Screen Recording permission (user-granted, visible in System Prefs) | Zoom, OBS, Cleanshot X |
| Windows 10+ | `IDXGIOutputDuplication` | No special permission for current session | Microsoft Teams, OBS |

**Key point**: DG uses the same APIs as OBS Studio and Zoom. It requests the same OS permissions as those trusted applications. The difference is consent transparency.

**macOS specifics**:
- Binary must be code-signed and notarized via Apple Developer Program
- `com.apple.security.screen-capture` entitlement in the provisioning profile
- TCC entry is user-visible in System Settings > Privacy & Security > Screen Recording
- User can revoke at any time; DG handles this gracefully (halts capture, logs reason)

**Wayland specifics**:
- `xdg-desktop-portal` flow presents an OS-native dialog: "Digital Ghost wants to share your screen"
- The portal returns a PipeWire stream file descriptor
- DG has no access to windows outside the granted stream
- Re-authorization required on each login (by default; configurable to remember)

**The binary is not hidden**:
- System tray icon is always visible during capture
- All captured windows are logged to `~/.local/share/digitalghost/dg.log`
- `digitalghost status` shows capture state, resource usage, and last 10 captured window titles

### 3.4 Semantic Pollution

**The problem**: A vector DB that contains both "YouTube thumbnail for cat video" and "AppSec roadmap review session" is not a memory system — it is noise. The retrieval quality degrades to zero as the noise ratio approaches 1.

**The solution** (`internal/filter/engagement.go`, `internal/filter/classifier.go`, `internal/graph/coherence.go`):

**Signal 1 — Engagement Score**

Engagement score is computed per frame-inference result:

```
engagement_score = dwell_weight × interaction_weight × content_class_weight
```

- `dwell_weight`: seconds the active window was foregrounded, normalized. A window visible for 30+ seconds gets weight 1.0. A flash-visible window gets 0.1.
- `interaction_weight`: 3.0× if user typed within 10s of frame; 2.0× if user scrolled; 1.5× if user clicked a non-navigation element; 1.0× if passive.
- `content_class_weight`: output of the fast classifier (see below).

Frames with `engagement_score < 0.3` are discarded. This threshold is configurable.

**Signal 2 — Fast Content Classifier**

Before invoking Ollama (expensive), a lightweight URL + window title classifier categorizes the frame:

| Category | Weight | Examples |
|----------|--------|---------|
| `work-document` | 1.0 | Google Docs, Office, Notion, PDFs |
| `work-code` | 1.0 | VS Code, terminals, GitHub PRs |
| `work-research` | 0.9 | HackerNews tech articles, arXiv, Stack Overflow |
| `communication` | 0.7 | Slack, email, Jira |
| `system-ui` | 0.2 | File manager, Settings, App installer |
| `entertainment` | 0.1 | YouTube, Netflix, Twitch, Steam |
| `social` | 0.15 | Twitter/X, Reddit (general), Instagram |
| `unknown` | 0.5 | Unclassified (let VLM decide) |

The classifier runs in < 1ms using string matching and domain lookup. It does not use ML.

**Signal 3 — Graph Coherence**

After Ollama generates a description, `coherence.go` checks:

1. Embed the description using a local embedding model (nomic-embed-text via Ollama).
2. Query LanceDB for the nearest K existing nodes.
3. If max cosine similarity > `graph_coherence_threshold` (default: 0.4), the new node connects to the graph and is stored with normal engagement threshold.
4. If max cosine similarity < threshold (isolated node), the node requires `engagement_score ≥ 0.8` to be stored. A truly isolated memory with no connections to existing knowledge is likely noise unless the user was highly engaged.

**Signal 4 — Temporal Deduplication**

If the same pHash (or a hash within Hamming distance 5) appears more than 3 times within 5 minutes, DG aggregates them into a single node with:
- Updated timestamp (last seen)
- Incremented `view_count`
- `engagement_score` boosted by `log(view_count)` (repeated views = higher relevance)

This prevents N identical nodes for a static document the user was reading.

**Together, these four signals mean**: A YouTube thumbnail generates a frame with `content_class_weight=0.1`, no user interaction, and no graph connections. Its engagement score is ~0.05, well below the 0.3 threshold. It never enters the DB. An AppSec roadmap the user spent 20 minutes reading, scrolled through, and typed notes about generates a score > 0.9 and is stored with high confidence.

---

## 4. The Golden Path Architecture Decision Record

| Decision | Options Considered | Chosen | Reason |
|----------|-------------------|--------|--------|
| Screen capture API | X11 XShm, XRecord, Wayland portal | XShm (X11) + Portal (Wayland) | XShm is established, zero-permission; Portal is the correct Wayland path |
| VLM provider | Ollama (local), Replicate (cloud), custom | Ollama | Air-gap requirement; Ollama is the de-facto local VLM standard |
| Vector DB | LanceDB, Qdrant, Chroma, pgvector | LanceDB | Embeddable (no server process); Apache Arrow; Rust core with Go bindings |
| Encryption | Full-disk encryption, DB-level, record-level | AES-256-GCM per record | Granular: can delete individual records securely; key management is independent of DB |
| Key storage | Env var, config file, OS keychain | OS keychain | Key never touches disk; OS keychain is hardware-backed on modern systems |
| Privacy gate placement | Post-capture, pre-VLM, pre-capture | Pre-capture (window metadata) | Zero cost; no pixel ever read for blocked windows |
| Resource governor | Process-level nice/ionice, cgroups, application-level | Application-level governor | Cross-platform; does not require root; more responsive than OS schedulers |

---

## 5. Implementation Sequencing

The sequencing is a safety constraint, not a preference. Each phase depends on the previous phase's safety properties being sound.

### Phase 0 — Foundation

**Nothing runs without this phase being complete and reviewed.**

- `internal/config/config.go` — Config struct with validation. Reject configs with `max_cpu_pct > 80` or `retention_days > 730`.
- `internal/tray/consent.go` — First-run consent dialog. Stores consent record. Capture goroutine will not start without a valid consent record. Tray icon init.
- `internal/storage/encrypt.go` — AES-256-GCM encryption, key derivation, per-record IV generation.
- `internal/storage/keychain.go` — OS keychain abstraction. On failure, fatal error (not fallback to file storage).

### Phase 1 — Privacy-Safe Capture

**This phase must be code-reviewed before Phase 2 is built.**

- `internal/capture/privacy.go` — Window metadata query + gate decision.
- `internal/filter/blocklist.go` — Blocklist engine with hot-reload and fail-closed behavior.
- `internal/capture/frame.go` — Frame struct + pHash computation.
- `internal/capture/capture_linux_x11.go` — First platform implementation.

### Phase 2 — Resource-Safe Inference

- `internal/inference/throttle.go` — Resource governor.
- `internal/inference/queue.go` — Bounded queue.
- `internal/inference/ollama.go` — Ollama client.

### Phase 3 — Semantic Quality

- `internal/filter/engagement.go` — Engagement scorer.
- `internal/filter/classifier.go` — Fast content classifier.
- `internal/graph/coherence.go` — Graph coherence.

### Phase 4 — Storage and Wiring

- `internal/storage/lancedb.go` — Vector store.
- `internal/storage/retention.go` — Retention + secure deletion.
- `cmd/digitalghost/main.go` — Wire everything; startup checks.

### Phase 5 — Additional Platforms

Wayland → macOS → Windows, in that order of complexity.

---

## 6. Minimum System Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| RAM | 8 GB | 16 GB |
| VRAM (GPU) | 4 GB (llava:7b) | 8 GB (llava:13b) |
| CPU | 4 cores | 8+ cores |
| Storage | 10 GB free | 50 GB free |
| OS | Linux (X11/Wayland), macOS 12.3+, Windows 10 | Linux with dedicated GPU |

---

## 7. Operational Properties

### What DG Logs

All capture decisions are logged to `~/.local/share/digitalghost/dg.log`:
- `BLOCKED: app=1password reason=password_manager_app_match`
- `BLOCKED: url=https://mybank.com/login reason=url_pattern:login`
- `PASS: app=code title="ARCHITECTURE.md — Digital-Ghost" engagement=0.87`
- `DROPPED: reason=duplicate pHash=... hamming=3`
- `STORED: node_id=abc123 embedding_dim=1536 score=0.87`

### What DG Does NOT Log

- Raw pixel data
- Screen content in any form
- Text extracted from screen (only VLM descriptions are stored)
- User keystrokes

### Performance Targets

| Metric | Target |
|--------|--------|
| Privacy gate latency | < 2ms per frame |
| pHash computation | < 1ms per frame |
| Capture-to-queue latency | < 5ms |
| Inference latency (llava:7b, idle GPU) | 5–15 seconds |
| Storage write latency | < 50ms |
| System CPU overhead (idle inference) | < 2% |
| Memory footprint (daemon, no inference) | < 50 MB |
