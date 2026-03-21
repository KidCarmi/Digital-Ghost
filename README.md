# Digital Ghost

**A local-only, air-gapped Contextual Memory Layer that indexes what you see.**

> "Don't be a supportive assistant. Be a gatekeeper."

This document begins with the honest assessment a Lead Architect and CISO owes before writing a single line of code.

---

## The Liability Brief (Read This First)

Before discussing how Digital Ghost works, here are the three reasons it can catastrophically fail — and the conditions under which it is safe to proceed.

### Liability #1 — Privacy-by-Accident Breach

DG captures screen content continuously. Inevitably, that screen will show a banking portal, a password manager's unlock screen, a private medical record, or an SSH private key displayed in a terminal. If any of that data enters the vector DB — even encrypted locally — it is now indexed and queryable. A single bug in the capture path is a credential exfiltration event, even with no network involved, because the attacker model now includes "attacker with physical access to the machine" and "malicious process that queries the DG API."

**Condition for proceeding:** A hard, fail-closed privacy gate must be the *first* component written and the *last* component changed. It must block capture based on window metadata (app name, URL) before any pixel is read. It must fail closed: if the gate cannot make a determination, it blocks.

### Liability #2 — Resource Predation

LLaVA-class Vision-Language Models require 4–8 GB VRAM and 100–200% CPU during inference. Without a governor that yields to foreground workloads, DG will peg the GPU continuously, drain a laptop battery in under 90 minutes, and cause thermal throttling on sustained use. Users will uninstall it within a day. **A tool that destroys the user experience provides no value.**

**Condition for proceeding:** A resource governor must be built before the inference pipeline is wired. It must enforce a configurable CPU/GPU budget and yield completely when the system is under foreground load.

### Liability #3 — Consent and Legal Exposure

On any system that is not exclusively single-user (a shared workstation, a corporate laptop, a device with a secondary OS user), capturing screen content without explicit per-session consent is a GDPR/CCPA violation and potentially an employment law violation. Even on a personal machine, capturing screen data from other users' X11 sessions on a shared Linux system is a breach.

**Condition for proceeding:** A first-run consent flow and a permanently-visible system tray indicator are required. DG must refuse to start without a verifiable consent record. No silent/headless mode is permitted.

---

## What Digital Ghost Is

```
┌──────────────────────────────────────────────────────────────────┐
│  LOCAL ONLY. NO NETWORK. ALL DATA STAYS ON YOUR MACHINE.        │
│  Air-gapped by design. Ollama runs locally. LanceDB is local.   │
└──────────────────────────────────────────────────────────────────┘
```

Digital Ghost is a background daemon that:

1. **Captures** your screen at a low frame rate (default: 2 fps)
2. **Filters** frames through a privacy gate (blocklist of sensitive apps/URLs)
3. **Deduplicates** using perceptual hashing (no repeated frames hit the AI)
4. **Describes** novel frames using a local Vision-Language Model via Ollama
5. **Scores** descriptions by engagement and semantic relevance
6. **Stores** high-value descriptions as encrypted vectors in a local LanceDB
7. **Exposes** a query interface: "What was I reading last Tuesday about distributed systems?"

It is explicitly **not**:
- A keylogger
- A cloud-sync tool
- A surveillance system
- An always-on recorder (it is a smart, filtered indexer)

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                   User's Desktop Session                │
│                                                         │
│  ┌──────────┐   window metadata    ┌─────────────────┐ │
│  │  OS APIs  │──(app, title, URL)──▶│  Privacy Gate   │ │
│  │ (XShm /   │                     │  (blocklist.go) │ │
│  │  Portal / │   raw frame         │        │        │ │
│  │  DXGI)    │────────────────────▶│  BLOCK / PASS   │ │
│  └──────────┘                      └────────┬────────┘ │
│                                             │ PASS only │
└─────────────────────────────────────────────┼──────────┘
                                              │
                     pHash dedup              ▼
                     (skip dupes)         ┌──────────┐
                                          │ FrameQ   │
                                          │(bounded) │
                                          └────┬─────┘
                                               │
                    ┌──────────────────────────▼──────────┐
                    │       Resource Governor              │
                    │  (throttle.go — CPU/GPU budget)      │
                    │  Yields when system is under load    │
                    └──────────────────────────┬──────────┘
                                               │
                    ┌──────────────────────────▼──────────┐
                    │        Ollama VLM Inference          │
                    │  (ollama.go — local HTTP, timeout)   │
                    │  Output: text description + tags     │
                    └──────────────────────────┬──────────┘
                                               │
                    ┌──────────────────────────▼──────────┐
                    │       Semantic Filter               │
                    │  Engagement score × Content class   │
                    │  Drop score < threshold (noise)     │
                    └──────────────────────────┬──────────┘
                                               │
                    ┌──────────────────────────▼──────────┐
                    │    Encrypted Vector Store           │
                    │  (LanceDB + AES-256-GCM per record) │
                    │  Key in OS keychain, never on disk  │
                    └─────────────────────────────────────┘
```

### Stack

| Component | Technology | Why |
|-----------|-----------|-----|
| Engine | Go 1.22 | Low-level OS APIs, goroutine concurrency model, single static binary |
| AI Inference | Ollama (llava:7b) | Fully local, no network, hardware-accelerated, swappable model |
| Vector Store | LanceDB | Embeddable, no server process, Apache Arrow columnar storage |
| Encryption | AES-256-GCM | Per-record AEAD; authenticated encryption prevents tampering |
| Key Storage | OS Keychain | libsecret (Linux), Keychain (macOS), DPAPI (Windows) |

---

## Repository Structure

```
Digital-Ghost/
├── cmd/digitalghost/main.go          # Entry point; wires all components
├── internal/
│   ├── config/config.go              # Validated configuration
│   ├── tray/consent.go               # First-run consent + always-visible indicator
│   ├── capture/
│   │   ├── privacy.go                # ★ Pre-capture privacy gate (highest consequence)
│   │   ├── frame.go                  # Frame struct + perceptual hash
│   │   ├── capture_linux_x11.go      # X11 XShmGetImage implementation
│   │   ├── capture_linux_wayland.go  # PipeWire / xdg-desktop-portal
│   │   ├── capture_darwin.go         # ScreenCaptureKit
│   │   └── capture_windows.go        # DXGI Desktop Duplication
│   ├── filter/
│   │   ├── blocklist.go              # Domain + app blocklist (hot-reload, fail-closed)
│   │   ├── engagement.go             # Dwell time + interaction scoring
│   │   └── classifier.go             # Fast content category classifier (pre-VLM)
│   ├── inference/
│   │   ├── throttle.go               # Resource governor (CPU/GPU budget)
│   │   ├── queue.go                  # Bounded frame queue with drop policy
│   │   └── ollama.go                 # Ollama HTTP client
│   ├── graph/
│   │   └── coherence.go              # Graph coherence scoring
│   └── storage/
│       ├── encrypt.go                # AES-256-GCM per-record encryption
│       ├── keychain.go               # OS keychain abstraction
│       ├── lancedb.go                # LanceDB vector store client
│       └── retention.go              # Retention policy + secure deletion
├── docs/
│   ├── ARCHITECTURE.md               # Full technical architecture
│   └── THREAT_MODEL.md               # STRIDE threat model
└── configs/
    ├── default.yaml                  # Default resource budget config
    └── blocklist.yaml                # Default sensitive app/domain blocklist
```

---

## Getting Started

### Prerequisites

- Go 1.22+
- [Ollama](https://ollama.com/) running locally with a vision model: `ollama pull llava:7b`
- Linux with X11 or Wayland, macOS 12.3+, or Windows 10+

### Build

```bash
go build -o bin/digitalghost ./cmd/digitalghost
```

### First Run

On first run, DG displays a consent dialog explaining what it captures and how to stop it. Capture does not begin until consent is given. A system tray icon is always visible when DG is running.

```bash
./bin/digitalghost start
```

### Wipe All Data

```bash
./bin/digitalghost wipe --confirm
```

This deletes all stored vectors and the consent record within 60 seconds.

---

## Security Properties

- **Local only**: No network calls except to `127.0.0.1:11434` (Ollama). Verifiable with `ss -tp` or `lsof -i`.
- **Encrypted at rest**: Every stored memory node is AES-256-GCM encrypted. The key lives in the OS keychain, never on disk.
- **Tamper-evident**: Each node carries an HMAC-SHA256; verified on every read.
- **Minimal privilege**: DG refuses to run as root. No setuid bit required.
- **Auditable**: All capture decisions (blocked/allowed) are logged to `~/.local/share/digitalghost/dg.log`.

See [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the full STRIDE analysis.

---

## License

MIT
