//go:build !windows

// Platform stubs for consent dialog and tray icon.
// Production implementations are in consent_linux.go, consent_darwin.go, consent_windows.go.
// This file provides the interface contract and a headless fallback for CI.
package tray

import (
	"fmt"
	"os"
	"runtime"
)

// startTrayIconImpl is implemented per-platform.
// See tray_linux.go, tray_darwin.go, tray_windows.go.
func startTrayIconImpl(onStop func()) error {
	// Stub: real implementations use systray or platform-native APIs.
	// Returns nil for headless/test environments.
	if os.Getenv("DG_HEADLESS_CONSENT") == "1" {
		return nil
	}
	return fmt.Errorf("tray not implemented for platform %q; set DG_HEADLESS_CONSENT=1 for testing", runtime.GOOS)
}

// showConsentDialogImpl is implemented per-platform.
func showConsentDialogImpl(text string) (bool, error) {
	return false, fmt.Errorf("consent dialog not implemented for platform %q", runtime.GOOS)
}
