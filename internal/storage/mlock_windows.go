//go:build windows

package storage

import (
	"fmt"
	"log/slog"
	"syscall"
	"unsafe"
)

var (
	modKernel32   = syscall.NewLazyDLL("kernel32.dll")
	virtualLock   = modKernel32.NewProc("VirtualLock")
	virtualUnlock = modKernel32.NewProc("VirtualUnlock")
)

// mlockKey calls VirtualLock to prevent key pages from being swapped.
// Unlike POSIX mlock, VirtualLock requires only user-level access on Windows;
// it does not need SE_LOCK_MEMORY privilege in most cases.
func mlockKey(key []byte, logger *slog.Logger) {
	if len(key) == 0 {
		return
	}
	r, _, err := virtualLock.Call(
		uintptr(unsafe.Pointer(&key[0])),
		uintptr(len(key)),
	)
	if r == 0 {
		logger.Warn("VirtualLock of encryption key failed (swap exposure risk)",
			"error", err)
	}
}

// munlockKey releases the VirtualLock on key pages.
func munlockKey(key []byte) error {
	if len(key) == 0 {
		return nil
	}
	r, _, err := virtualUnlock.Call(
		uintptr(unsafe.Pointer(&key[0])),
		uintptr(len(key)),
	)
	if r == 0 {
		return fmt.Errorf("VirtualUnlock: %w", err)
	}
	return nil
}
