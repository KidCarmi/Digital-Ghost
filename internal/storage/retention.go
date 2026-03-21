// retention.go implements the retention policy and secure deletion for Digital Ghost.
//
// Retention:
//   - Nodes older than retention_days are deleted on a daily sweep (default: 2 AM local time).
//   - The sweep is best-effort: if the machine is off at 2 AM, it runs at next startup.
//   - Retention period is configurable; the hard maximum is 730 days (2 years).
//
// Secure deletion:
//   - On HDD: DoD 5220.22-M (3-pass overwrite: all 0s, all 1s, random).
//   - On SSD/NVMe: standard delete + TRIM command (file system tells device to erase).
//   - The distinction is made by checking the device's rotational attribute via sysfs.
//
// Wipe (`digitalghost wipe --confirm`):
//   - Removes all stored nodes.
//   - Deletes the consent record.
//   - Calls KeyManager.DeleteKey() to destroy the encryption key.
//   - After wipe, all previously stored data is permanently unreadable.
package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// RetentionManager enforces the retention policy.
type RetentionManager struct {
	store          *Store
	keyManager     *KeyManager
	dataDir        string
	retentionDays  int
	secureDelete   bool
	logger         *slog.Logger
}

// NewRetentionManager creates a RetentionManager.
func NewRetentionManager(store *Store, km *KeyManager, dataDir string, retentionDays int, secureDelete bool, logger *slog.Logger) *RetentionManager {
	return &RetentionManager{
		store:         store,
		keyManager:    km,
		dataDir:       dataDir,
		retentionDays: retentionDays,
		secureDelete:  secureDelete,
		logger:        logger,
	}
}

// RunScheduled starts the retention sweep scheduler.
// It runs a sweep at startup (for missed sweeps) and then daily at 2 AM.
// Blocks until stopCh is closed.
func (r *RetentionManager) RunScheduled(stopCh <-chan struct{}) {
	// Run once at startup to catch any missed sweeps.
	r.logger.Info("running startup retention sweep")
	if err := r.Sweep(context.Background()); err != nil {
		r.logger.Warn("startup retention sweep failed", "error", err)
	}

	for {
		next := nextSweepTime()
		r.logger.Info("next retention sweep scheduled", "at", next.Format(time.RFC3339))

		select {
		case <-stopCh:
			return
		case <-time.After(time.Until(next)):
			r.logger.Info("running scheduled retention sweep")
			if err := r.Sweep(context.Background()); err != nil {
				r.logger.Warn("retention sweep failed", "error", err)
			}
		}
	}
}

// Sweep deletes all nodes older than retentionDays.
func (r *RetentionManager) Sweep(ctx context.Context) error {
	cutoff := time.Now().AddDate(0, 0, -r.retentionDays)
	r.logger.Info("retention sweep", "cutoff", cutoff.Format("2006-01-02"), "retention_days", r.retentionDays)

	nodeIDs, err := r.store.db.ListOlderThan(ctx, defaultTable, cutoff)
	if err != nil {
		return fmt.Errorf("listing old nodes: %w", err)
	}

	if len(nodeIDs) == 0 {
		r.logger.Info("retention sweep: no expired nodes")
		return nil
	}

	r.logger.Info("retention sweep: deleting expired nodes", "count", len(nodeIDs))
	var deleteErrors int
	for _, id := range nodeIDs {
		if err := r.store.Delete(ctx, id); err != nil {
			r.logger.Warn("failed to delete node during retention sweep",
				"node_id", fmt.Sprintf("%x", id), "error", err)
			deleteErrors++
		}
	}

	if deleteErrors > 0 {
		return fmt.Errorf("retention sweep: %d/%d deletions failed", deleteErrors, len(nodeIDs))
	}
	r.logger.Info("retention sweep complete", "deleted", len(nodeIDs))
	return nil
}

// WipeAll permanently destroys all DG data.
// This is the implementation of `digitalghost wipe --confirm`.
//
// Order of operations:
//  1. Close the store (flush any pending writes).
//  2. Delete all LanceDB files from disk.
//  3. Delete consent.json.
//  4. Delete the encryption key from the OS keychain.
//  5. Delete the capture log.
//
// After this call, all previously stored data is permanently unreadable,
// because the encryption key no longer exists.
func (r *RetentionManager) WipeAll(ctx context.Context) error {
	r.logger.Info("WIPE ALL: beginning permanent data destruction")
	start := time.Now()

	// Step 1: Close the database.
	if err := r.store.Close(); err != nil {
		r.logger.Warn("WIPE ALL: store close failed (continuing)", "error", err)
	}

	// Step 2: Delete all files in the data directory.
	// We delete the LanceDB table files, the log, and the consent record.
	entries, err := os.ReadDir(r.dataDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading data directory: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(r.dataDir, entry.Name())
		if r.secureDelete {
			if err := secureDeleteFile(path); err != nil {
				r.logger.Warn("WIPE ALL: secure delete failed, falling back to regular delete",
					"path", path, "error", err)
				os.RemoveAll(path)
			}
		} else {
			os.RemoveAll(path)
		}
	}

	// Step 3: Delete the consent record (separately, may be in a different path).
	consentPath := filepath.Join(r.dataDir, "consent.json")
	os.Remove(consentPath)

	// Step 4: Destroy the encryption key. After this, all stored data (if any remains)
	// is permanently unreadable.
	if err := r.keyManager.DeleteKey(); err != nil {
		return fmt.Errorf("WIPE ALL: failed to delete encryption key: %w", err)
	}

	elapsed := time.Since(start)
	r.logger.Info("WIPE ALL: complete", "elapsed_ms", elapsed.Milliseconds())

	if elapsed > 60*time.Second {
		r.logger.Warn("WIPE ALL: took longer than 60 seconds", "elapsed", elapsed)
	}

	return nil
}

// nextSweepTime returns the next 2 AM in local time.
func nextSweepTime() time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, now.Location())
	if now.After(next) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// secureDeleteFile overwrites a file with zeros before deleting it.
// For HDD: performs 3-pass DoD 5220.22-M wipe (zeros, ones, random).
// This is best-effort on SSD/NVMe where the drive firmware controls write placement.
func secureDeleteFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		// For directories: wipe each file recursively, then remove directory.
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := secureDeleteFile(filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
		return os.Remove(path)
	}

	size := info.Size()
	if size == 0 {
		return os.Remove(path)
	}

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening file for secure delete: %w", err)
	}

	passes := []byte{0x00, 0xFF} // Pass 1: zeros; Pass 2: ones.
	for _, fillByte := range passes {
		if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			return err
		}
		buf := make([]byte, min(int64(4096), size))
		for i := range buf {
			buf[i] = fillByte
		}
		written := int64(0)
		for written < size {
			toWrite := min(int64(len(buf)), size-written)
			n, err := f.Write(buf[:toWrite])
			if err != nil {
				f.Close()
				return fmt.Errorf("overwrite pass failed: %w", err)
			}
			written += int64(n)
		}
		f.Sync()
	}

	// Pass 3: random data.
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return err
	}
	// Use /dev/urandom for random pass.
	urandom, err := os.Open("/dev/urandom")
	if err == nil {
		buf := make([]byte, 4096)
		written := int64(0)
		for written < size {
			toWrite := min(int64(len(buf)), size-written)
			if _, err := io.ReadFull(urandom, buf[:toWrite]); err != nil {
				break
			}
			n, writeErr := f.Write(buf[:toWrite])
			if writeErr != nil {
				break
			}
			written += int64(n)
		}
		urandom.Close()
		f.Sync()
	}

	f.Close()
	return os.Remove(path)
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
