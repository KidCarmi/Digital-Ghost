//go:build !windows

package storage

import (
	"fmt"
	"log/slog"
	"syscall"
)

// mlockKey calls mlock(2) to prevent the key pages from being swapped to disk.
// This is best-effort: if RLIMIT_MEMLOCK is too low (common for non-root users),
// mlock returns EPERM; DG logs a warning but continues — not a fatal error.
//
// Why: the OS can swap process memory pages to disk at any time. Without mlock,
// key material may appear in the swap file/partition in plaintext, surviving
// process exit and persisting until that swap sector is overwritten.
func mlockKey(key []byte, logger *slog.Logger) {
	if err := syscall.Mlock(key); err != nil {
		logger.Warn("mlock of encryption key failed (swap exposure risk)",
			"error", err,
			"hint", "raise RLIMIT_MEMLOCK or run with CAP_IPC_LOCK to prevent key swap")
	}
}

// munlockKey releases the mlock on key pages and is called from Close().
func munlockKey(key []byte) error {
	if err := syscall.Munlock(key); err != nil {
		return fmt.Errorf("munlock: %w", err)
	}
	return nil
}
