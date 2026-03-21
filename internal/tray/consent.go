// Package tray manages the first-run consent flow and the always-visible
// system tray indicator. Nothing in the DG daemon may start capturing
// until IsConsentGranted() returns true.
//
// Design constraints:
//   - Consent cannot be inferred or assumed; it must be explicitly granted.
//   - The tray icon must be visible whenever DG is running.
//   - DG must halt gracefully if the tray icon cannot be shown.
package tray

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// showConsentDialog presents the consent dialog to the user and returns
// true if they explicitly granted consent.
func showConsentDialog(text string) (granted bool, err error) {
	if os.Getenv("DG_HEADLESS_CONSENT") == "1" {
		fmt.Fprintln(os.Stderr, "WARNING: DG_HEADLESS_CONSENT=1 bypassing consent dialog (test mode only)")
		return true, nil
	}
	return showConsentDialogImpl(text)
}

// currentPlatform returns a string identifying the current OS.
func currentPlatform() string {
	return runtime.GOOS
}

// consentDialogText is the canonical text of the consent dialog.
// Its SHA-256 hash is stored in consent.json so we can detect if the
// consent was granted under a different version of the dialog.
const consentDialogText = `Digital Ghost will run in the background and capture
screenshots of your screen at a low frame rate (default: 2 frames per second).

These screenshots are:
  • Processed locally by an AI model on YOUR computer
  • Never sent to any server or external service
  • Stored in an encrypted local database
  • Used only to power a personal memory search feature

Digital Ghost ALSO:
  • Captures all connected monitors (multi-monitor setups are fully recorded)
  • Reads the URL from your browser's address bar when a browser is the active window
    (supported browsers: Chrome, Edge, Firefox, Opera, Brave, Vivaldi, Chromium)
  • Measures keyboard/mouse activity to assess engagement (no keystrokes are recorded,
    only the time elapsed since the last input event)

Digital Ghost will NOT capture:
  • Password managers (1Password, Bitwarden, KeePass, etc.)
  • Banking or financial websites
  • Login / authentication screens
  • Medical records portals

You can stop Digital Ghost at any time by:
  • Right-clicking the tray icon and selecting "Stop"
  • Running: digitalghost stop
  • Wiping all data: digitalghost wipe --confirm

A tray icon (◉) will always be visible while Digital Ghost is running.
There is no silent or hidden mode.

Do you agree to allow Digital Ghost to capture your screen?`

// ConsentRecord is serialized to consent.json.
// It records who consented, when, under what version of the dialog.
type ConsentRecord struct {
	GrantedAt      time.Time `json:"granted_at"`
	DGVersion      string    `json:"dg_version"`
	Platform       string    `json:"platform"`
	DialogTextHash string    `json:"dialog_text_hash"` // SHA-256 of consentDialogText
	ExplicitGrant  bool      `json:"explicit_grant"`   // true = user clicked "I Agree"
}

// Manager handles consent state and the tray icon lifecycle.
type Manager struct {
	dataDir string
	version string
}

// New creates a Manager. dataDir is the DG data directory (e.g., ~/.local/share/digitalghost).
func New(dataDir, version string) *Manager {
	return &Manager{dataDir: dataDir, version: version}
}

// consentPath returns the path to the consent record file.
func (m *Manager) consentPath() string {
	return filepath.Join(m.dataDir, "consent.json")
}

// IsConsentGranted returns true if a valid consent record exists on disk.
// It does NOT interactively prompt — call RequestConsent() for that.
func (m *Manager) IsConsentGranted() bool {
	record, err := m.loadConsentRecord()
	if err != nil {
		return false
	}
	return record.ExplicitGrant && record.DialogTextHash == dialogHash()
}

// RequestConsent shows the consent dialog to the user and, if they agree,
// writes a consent record. Returns an error if consent is denied or the
// dialog cannot be shown.
//
// This is a blocking call. It must be called from the main goroutine on
// platforms that require UI operations on the main thread.
func (m *Manager) RequestConsent() error {
	if err := os.MkdirAll(m.dataDir, 0700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	// Show the consent dialog via the platform abstraction.
	granted, err := showConsentDialog(consentDialogText)
	if err != nil {
		return fmt.Errorf("showing consent dialog: %w", err)
	}
	if !granted {
		return errors.New("user denied consent; Digital Ghost will not start")
	}

	record := ConsentRecord{
		GrantedAt:      time.Now().UTC(),
		DGVersion:      m.version,
		Platform:       currentPlatform(),
		DialogTextHash: dialogHash(),
		ExplicitGrant:  true,
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling consent record: %w", err)
	}

	// Write atomically: write to a temp file, then rename.
	tmp := m.consentPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing consent record: %w", err)
	}
	if err := os.Rename(tmp, m.consentPath()); err != nil {
		return fmt.Errorf("committing consent record: %w", err)
	}

	return nil
}

// RevokeConsent deletes the consent record. The daemon will halt on its
// next startup check. Currently-running capture goroutines are not
// immediately halted by this function — the caller must stop the daemon.
func (m *Manager) RevokeConsent() error {
	err := os.Remove(m.consentPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil // Already revoked.
	}
	return err
}

// loadConsentRecord reads and parses the consent record from disk.
func (m *Manager) loadConsentRecord() (*ConsentRecord, error) {
	data, err := os.ReadFile(m.consentPath())
	if err != nil {
		return nil, err
	}
	var record ConsentRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("corrupt consent record: %w", err)
	}
	if !record.ExplicitGrant {
		return nil, errors.New("consent record does not contain explicit grant")
	}
	return &record, nil
}

// dialogHash returns the SHA-256 hex hash of the current consent dialog text.
func dialogHash() string {
	h := sha256.Sum256([]byte(consentDialogText))
	return hex.EncodeToString(h[:])
}

// StartTrayIcon launches the system tray icon. It must be called after consent
// is granted and before capture begins. Returns an error if the tray cannot
// be initialized (e.g., headless environment without a display server).
//
// The tray icon remains visible for the lifetime of the process. If it
// disappears (e.g., compositor crash), the capture loop must be halted.
func StartTrayIcon(onStop func()) error {
	return startTrayIconImpl(onStop)
}
