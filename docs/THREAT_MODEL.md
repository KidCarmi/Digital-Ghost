# Digital Ghost — Threat Model

**Methodology**: STRIDE
**Scope**: The DG daemon process, its local data store, and its interaction with the OS and Ollama.
**Out of scope**: Network threats (DG has no network surface), multi-tenant environments (DG is single-user by design), attacks on Ollama itself (treated as a trusted local component).

---

## 1. Assets

| Asset | Sensitivity | Location |
|-------|------------|---------|
| Captured screen descriptions | High — may contain work context, meeting content, research | `~/.local/share/digitalghost/*.lance` (encrypted) |
| Encryption key | Critical — compromise = all stored data readable | OS keychain (libsecret/Keychain/DPAPI) |
| Blocklist configuration | High — if corrupted, sensitive windows may be captured | `~/.config/digitalghost/blocklist.yaml` |
| Consent record | Medium — if deleted, user consent state is lost | `~/.local/share/digitalghost/consent.json` |
| Capture log | Medium — reveals which windows were captured/blocked | `~/.local/share/digitalghost/dg.log` |
| DG binary | Medium — if replaced, all other controls are bypassed | Installation path |

---

## 2. Trust Boundaries

```
┌─────────────────────────────────────────────────────┐
│  User Session (trusted)                             │
│  ┌───────────────┐     ┌───────────────────────┐    │
│  │  DG Daemon    │────▶│  OS Keychain          │    │
│  │  (our code)   │     │  (OS-managed, trusted)│    │
│  └───────┬───────┘     └───────────────────────┘    │
│          │ 127.0.0.1:11434                           │
│  ┌───────▼───────┐     ┌───────────────────────┐    │
│  │  Ollama       │     │  LanceDB files         │    │
│  │  (local proc) │     │  (encrypted on disk)  │    │
│  └───────────────┘     └───────────────────────┘    │
│          │                                          │
│   ════════════════ TRUST BOUNDARY ════════════════  │
│          │                                          │
│  ┌───────▼───────────────────────────────────────┐ │
│  │  Other processes on the same machine          │ │
│  │  (untrusted — may attempt to read DG data     │ │
│  │   or influence DG behavior)                   │ │
│  └───────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

---

## 3. STRIDE Analysis

### 3.1 Spoofing

**S1 — Malicious app spoofs window metadata to suppress capture**

- **Vector**: A malicious process sets its window title to match a blocklist pattern (e.g., "1Password") to prevent DG from capturing its activity.
- **Impact**: DG's own privacy protection is turned against it; DG fails to capture content the user wanted indexed.
- **Likelihood**: Low. Requires the attacker to know DG's blocklist and to have a process already running.
- **Mitigation**: Verify process identity by PID against `/proc/<pid>/exe` (Linux) or kernel process table, not just window title. A window titled "1Password" but whose PID maps to `malware.bin` will not match the `apps` blocklist (which checks process name, not window title).
- **Residual risk**: Accepted. A spoofed title preventing capture is a denial-of-service against DG's indexing, not a privacy breach.

**S2 — Compromised Ollama serves malicious model output**

- **Vector**: An attacker replaces or tampers with the Ollama model, causing it to output crafted descriptions designed to inject false memories or manipulate retrieval.
- **Impact**: The knowledge graph is poisoned; retrieval returns attacker-controlled content.
- **Likelihood**: Very low. Requires physical access to the machine or code execution as the user.
- **Mitigation**: DG verifies Ollama model SHA256 hash on startup against a pinned value in config. If hash mismatch, DG logs a warning and disables inference (does not halt capture). Model integrity check is configurable.

---

### 3.2 Tampering

**T1 — Direct modification of LanceDB files on disk**

- **Vector**: An attacker with filesystem access modifies the encrypted Lance files to inject false memory nodes or delete real ones.
- **Impact**: False memories injected; real memories deleted.
- **Likelihood**: Low. Requires local access as the user or a process running as the user.
- **Mitigation**: Every MemoryNode includes an HMAC-SHA256 over `(node_id || timestamp || content_vector || description)` computed with the same key used for encryption. Verification occurs on every read. Tampered nodes return an error and are quarantined (moved to `.quarantine/`, not deleted, for forensic review).
- **Residual risk**: An attacker who has the encryption key (from the OS keychain) can forge valid HMACs. This is accepted: if the attacker has the OS keychain, they have full access to the user's session and DG is not the binding security control.

**T2 — Hot-reload race condition in blocklist**

- **Vector**: An attacker modifies `blocklist.yaml` to remove a sensitive entry (e.g., banking URLs) in the window between hot-reload events, causing a banking session to be captured.
- **Impact**: Sensitive content captured and stored.
- **Likelihood**: Very low. Requires timing precision and write access to user config.
- **Mitigation**: Blocklist hot-reload uses atomic file replacement (rename-based swap). The in-memory blocklist is updated only after the new file is fully parsed and validated. If the new file is invalid, the old blocklist remains active (fail-safe). File permissions on `blocklist.yaml` are set to `0600` (owner read/write only) on first write.

---

### 3.3 Repudiation

**R1 — User denies content was captured**

- **Vector**: User claims DG captured sensitive content; DG has no record.
- **Impact**: Audit trail gap.
- **Likelihood**: N/A (this is a design property).
- **Mitigation**: The capture log (`dg.log`) is append-only with timestamps. Every PASS and BLOCK decision is logged. Log rotation preserves history up to the configured retention period. Log files are not encrypted (they contain only metadata, not content).

**R2 — User denies giving consent**

- **Vector**: User claims they never consented to DG running.
- **Impact**: Legal/compliance exposure.
- **Likelihood**: Low if consent flow is implemented correctly.
- **Mitigation**: Consent record stored in `consent.json` includes: timestamp, DG version, platform, a hash of the consent dialog text shown, and whether the user explicitly clicked "I Agree" (vs. press-any-key). The daemon verifies this record on every startup. The record cannot be regenerated without re-running the consent flow.

---

### 3.4 Information Disclosure

**I1 — Encrypted LanceDB exfiltration**

- **Vector**: Attacker copies `~/.local/share/digitalghost/*.lance` to another machine and attempts to decrypt.
- **Impact**: All stored screen descriptions readable.
- **Likelihood**: Medium. File copy requires user-level access (or physical access).
- **Mitigation**: AES-256-GCM key is stored in the OS keychain, never in the LanceDB files or any config file. The keychain entry is bound to the current user session. On Linux, libsecret is backed by gnome-keyring or kwallet, which require the user's login password to unlock. On macOS, the Keychain is hardware-backed on T2/M-series chips. Without the key, the LanceDB files are opaque ciphertext.
- **Residual risk**: If the attacker has the user's login password AND physical access, they can derive the keychain key. This is accepted as out-of-scope (full user account compromise).

**I2 — DG log file disclosure**

- **Vector**: Attacker reads `dg.log`, which contains a record of every captured window title and URL.
- **Impact**: Activity metadata leak (which apps were open, which URLs were visited).
- **Likelihood**: Medium. Log file is in `~/.local/share/`, readable by the user and root.
- **Mitigation**: Log files are `0600`. Consider encrypting logs in a future version. For now, the log contains only metadata (no screen content) and is in the same location as the LanceDB files — if either is compromised, the other is too.

**I3 — Memory disclosure via swap**

- **Vector**: Decrypted frame buffers or VLM descriptions may be written to swap (disk) by the OS, where they persist after process exit.
- **Impact**: Sensitive screen content on disk in plaintext.
- **Likelihood**: Low-medium on systems with swap enabled.
- **Mitigation**: Frame buffers are allocated with `mlock()` where available (Linux/macOS) to prevent them from being swapped. VLM description strings are zeroed after encryption using `runtime.SetFinalizer` on the containing struct. On systems where `mlock()` fails (insufficient `RLIMIT_MEMLOCK`), DG logs a warning but does not halt (full mlock would require elevated privileges, which we refuse to take).

---

### 3.5 Denial of Service

**D1 — Malicious app floods the capture queue**

- **Vector**: A process rapidly creates and destroys windows with titles that pass the blocklist, forcing DG to allocate frame buffers continuously.
- **Impact**: DG's queue fills; legitimate frames are dropped; memory pressure increases.
- **Likelihood**: Low. Requires a malicious process in the same user session.
- **Mitigation**: Per-app rate limiting in the capture engine. If a given process ID is responsible for > 10 frame-eligible events per second, it is rate-limited to 1 frame per second. The FrameQueue uses a fixed-size ring buffer (no dynamic allocation); overflow drops the oldest frame rather than growing memory.

**D2 — Malformed Ollama response hangs inference loop**

- **Vector**: A corrupted or adversarially crafted Ollama response causes the inference goroutine to block indefinitely.
- **Impact**: Inference loop stalls; queue fills; all subsequent capture is dropped.
- **Mitigation**: Every Ollama HTTP call has a `timeout_sec` (default: 30s) context deadline. On timeout, the frame is discarded and the goroutine proceeds. A watchdog goroutine monitors time-since-last-inference; if > 2× `timeout_sec`, it logs an error and restarts the inference goroutine.

---

### 3.6 Elevation of Privilege

**E1 — DG used to escalate OS privileges**

- **Vector**: DG binary is setuid or runs as root; attacker exploits a DG vulnerability to gain root.
- **Impact**: Full system compromise.
- **Likelihood**: Low if mitigations are in place.
- **Mitigation**: DG checks `os.Getuid() == 0` on startup and exits with a fatal error if running as root. The binary must never be installed with setuid bit. The install script verifies and refuses to set setuid. No capabilities (Linux `CAP_*`) are requested or needed.

**E2 — DG binary replacement**

- **Vector**: Attacker replaces the DG binary with a malicious version that bypasses all privacy controls.
- **Impact**: All safety properties lost.
- **Likelihood**: Low. Requires write access to the installation path.
- **Mitigation**: This is outside DG's control. Recommendation: install DG in a directory owned by root with `0755` permissions (world-readable, root-writable only). Add binary hash verification to a startup script or systemd service file. On macOS, notarization and Gatekeeper provide this. On Linux, consider `dm-verity` for the installation partition on high-security setups.

---

## 4. Attack Scenarios

### Scenario A: Compromised Ollama Instance

**Threat**: An attacker exploits a vulnerability in Ollama to gain code execution in the Ollama process, then manipulates inference results to exfiltrate screen descriptions.

**DG's position**: DG sends pixel data to Ollama and receives text. If Ollama is compromised:
- The attacker can read frame content as it passes through (DG cannot prevent this)
- The attacker can inject false descriptions (mitigated by graph coherence scoring, which flags descriptions that are statistically anomalous)

**Recommendation**: Ollama should be sandboxed (e.g., in a separate user account or container). This is a deployment concern, not a DG concern, but should be documented for users.

### Scenario B: DB Extraction and Offline Attack

**Threat**: Attacker copies the LanceDB files and the OS keychain export, then performs offline decryption.

**DG's position**: If the attacker has both the LanceDB files and the keychain entry (which requires the user's login password), decryption is trivial. This is accepted. DG's encryption protects against:
- File copy without keychain (laptop theft, disk clone without login)
- Cold-boot attacks (key is in keychain, not in memory after process exit)

### Scenario C: Malicious Model Injection

**Threat**: Attacker replaces `llava:7b` in the Ollama model store with a trojanized version that includes a covert channel.

**DG's position**: Model SHA256 verification on startup detects this. If verification is disabled, DG has no other defense — this is equivalent to compromising the Ollama binary. Recommendation: pin model hash in config and treat hash changes as security events.

---

## 5. Data Residency

All data created by DG resides exclusively in:

```
~/.local/share/digitalghost/     (Linux)
~/Library/Application Support/DigitalGhost/  (macOS)
%APPDATA%\DigitalGhost\          (Windows)
```

**What DG writes**:
- `db/` — Encrypted LanceDB vector store
- `dg.log` — Capture decision log (metadata only, not content)
- `consent.json` — Consent record
- `.quarantine/` — Tampered nodes (if any detected)

**What DG never writes**:
- Raw pixel data
- Screenshot files
- Plaintext descriptions
- Encryption keys (key lives in OS keychain)

**Network**: DG makes no outbound network connections. All Ollama communication is via `127.0.0.1:11434`. This can be verified with:
```
# Linux
ss -tp | grep digitalghost

# macOS
lsof -i -n -P | grep digitalghost
```

---

## 6. Retention and Erasure

| Operation | Command | Time | Method |
|-----------|---------|------|--------|
| Wipe all data | `digitalghost wipe --confirm` | < 60s | DoD 5220.22-M (HDD) or TRIM (SSD) |
| Delete single node | `digitalghost forget --node <id>` | < 1s | Secure overwrite + LanceDB delete |
| Revoke consent | `digitalghost revoke-consent` | < 1s | Deletes consent.json; halts daemon |
| Reduce retention | Edit `config.yaml` `retention_days` | < 5min | Next retention sweep removes old nodes |

**Automatic retention**: A daily sweep deletes nodes older than `retention_days` (default: 90). Deletion uses secure overwrite. The sweep runs at 2 AM local time when the system is likely idle.

---

## 7. Incident Response

**If you suspect DG has captured sensitive data:**

1. Stop the daemon immediately: `digitalghost stop`
2. Review the capture log: `tail -1000 ~/.local/share/digitalghost/dg.log | grep PASS`
3. Check for specific nodes: `digitalghost search --query "banking" --dry-run`
4. Delete specific nodes: `digitalghost forget --query "banking"`
5. If uncertain, wipe everything: `digitalghost wipe --confirm`
6. Review and tighten the blocklist: `~/.config/digitalghost/blocklist.yaml`
7. Restart with updated blocklist: `digitalghost start`

**Expected response time from "I think something was captured" to "data is gone"**: < 5 minutes.

---

## 8. Security Design Principles

1. **Fail closed**: When uncertain, block. Never fail open.
2. **Minimal privilege**: No root, no setuid, no kernel modules, no capabilities beyond what X11/Wayland/TCC grants to any screen recording app.
3. **Local only**: No network surface = no remote attacker surface.
4. **Transparency**: Every capture decision is logged. Users can always answer "what did DG see?"
5. **Erasure first**: The wipe command is a first-class feature, not an afterthought.
6. **Defense in depth**: Privacy protection has three layers (window metadata, input type detection, post-capture regex). Any one layer failing does not collapse the others.
