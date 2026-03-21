// jsonstore.go implements the lanceDBConn interface using a flat directory of
// encrypted JSON files — one file per MemoryNode.
//
// This replaces the LanceDB dependency (which has no published Go module) with
// a simple, dependency-free store that is good for up to ~10,000 nodes.
//
// Layout:
//
//	<dataDir>/memories/<hex-node-id>.enc   — encrypted MemoryNode payload
//	<dataDir>/memories/index.json          — plaintext index: {id, timestamp} for fast listing
//
// Vector similarity search loads all embeddings into memory and computes
// cosine distance. This is fine for the expected dataset size (<10k nodes).
package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/graph"
)

// indexEntry is a single row in the plaintext index file.
type indexEntry struct {
	ID        string    `json:"id"`        // hex-encoded [16]byte node ID
	Timestamp time.Time `json:"timestamp"` // capture timestamp
}

// JSONStore implements lanceDBConn using flat encrypted files.
type JSONStore struct {
	dir       string
	encryptor *Encryptor
	logger    *slog.Logger
	mu        sync.RWMutex
}

// indexSentinelID is the fixed NodeID used as authenticated context when
// encrypting the index file. It must never be reused for a real MemoryNode.
var indexSentinelID = [16]byte{
	0x44, 0x47, 0x2d, 0x49, 0x44, 0x58, // "DG-IDX"
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// indexSentinelTime is the fixed timestamp used as authenticated context for
// the index file HMAC. Using a constant means we can always reconstruct the
// EncryptedRecord needed by Open() without storing extra metadata.
var indexSentinelTime = time.Unix(0, 0).UTC()

// NewJSONStore creates a JSONStore rooted at dir/memories/.
// The directory is created if it does not exist.
// If a plaintext index.json exists from a previous version, it is migrated
// to the encrypted index.enc format on first open.
func NewJSONStore(dir string, enc *Encryptor, logger *slog.Logger) (*JSONStore, error) {
	memoriesDir := filepath.Join(dir, "memories")
	if err := os.MkdirAll(memoriesDir, 0700); err != nil {
		return nil, fmt.Errorf("creating memories directory: %w", err)
	}
	s := &JSONStore{dir: memoriesDir, encryptor: enc, logger: logger}

	// Migrate plaintext index.json → encrypted index.enc if needed.
	if err := s.migrateIndexIfNeeded(); err != nil {
		logger.Warn("index migration failed (will rebuild on next write)", "error", err)
	}
	return s, nil
}

// migrateIndexIfNeeded converts a legacy plaintext index.json to the
// encrypted index.enc format, then removes the plaintext file.
func (s *JSONStore) migrateIndexIfNeeded() error {
	plainPath := filepath.Join(s.dir, "index.json")
	encPath := s.indexEncPath()

	// Nothing to migrate if the plaintext file doesn't exist.
	if _, err := os.Stat(plainPath); os.IsNotExist(err) {
		return nil
	}
	// Encrypted index already exists — plaintext is a stale artifact, remove it.
	if _, err := os.Stat(encPath); err == nil {
		s.logger.Info("removing stale plaintext index.json (encrypted index.enc exists)")
		return os.Remove(plainPath)
	}

	// Read the plaintext index.
	data, err := os.ReadFile(plainPath)
	if err != nil {
		return fmt.Errorf("reading plaintext index: %w", err)
	}
	var entries []indexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parsing plaintext index: %w", err)
	}

	// Write the encrypted version.
	if err := s.saveIndexEntries(entries); err != nil {
		return fmt.Errorf("writing encrypted index: %w", err)
	}

	// Remove the plaintext file.
	if err := os.Remove(plainPath); err != nil {
		s.logger.Warn("could not remove plaintext index.json after migration", "error", err)
	}
	s.logger.Info("index.json migrated to encrypted index.enc", "entries", len(entries))
	return nil
}

// -- lanceDBConn interface --------------------------------------------------

func (s *JSONStore) WriteRecord(_ context.Context, _ string, nodeID [16]byte, ts time.Time, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.nodePath(nodeID)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		return fmt.Errorf("writing node file: %w", err)
	}
	if err := s.appendIndex(nodeID, ts); err != nil {
		s.logger.Warn("index update failed (non-fatal)", "error", err)
	}
	return nil
}

func (s *JSONStore) ReadRecord(_ context.Context, _ string, nodeID [16]byte) ([]byte, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.nodePath(nodeID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, fmt.Errorf("node %x not found", nodeID)
		}
		return nil, time.Time{}, fmt.Errorf("reading node file: %w", err)
	}

	ts := s.timestampFromIndex(nodeID)
	return data, ts, nil
}

func (s *JSONStore) NearestNeighbors(_ context.Context, _ string, query []float32, k int) ([]graph.ScoredNode, error) {
	if len(query) == 0 {
		return nil, nil
	}

	// Load the index under the read lock, then release before doing disk I/O.
	// Holding RLock across thousands of os.ReadFile calls would block all
	// concurrent writes for the full search duration (potentially 10+ seconds).
	s.mu.RLock()
	entries, err := s.loadIndex()
	s.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("loading index: %w", err)
	}

	type candidate struct {
		id    [16]byte
		score float64
	}
	var candidates []candidate

	for _, e := range entries {
		idBytes, err := hex.DecodeString(e.ID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		var nodeID [16]byte
		copy(nodeID[:], idBytes)

		// Read individual node files outside the lock. Each .enc file is
		// written atomically (write to .tmp + rename), so a concurrent write
		// either completes before we read (we see new data) or after (we skip).
		data, err := os.ReadFile(s.nodePath(nodeID))
		if err != nil {
			continue
		}

		rec := &EncryptedRecord{NodeID: nodeID, Timestamp: e.Timestamp, Data: data}
		plaintext, err := s.encryptor.Open(rec)
		if err != nil {
			s.logger.Warn("tampered or unreadable node skipped", "id", e.ID, "error", err)
			continue
		}

		var node MemoryNode
		if err := json.Unmarshal(plaintext, &node); err != nil {
			continue
		}
		if len(node.Embedding) == 0 {
			continue
		}

		sim := cosineSimilarity(query, node.Embedding)
		candidates = append(candidates, candidate{id: nodeID, score: sim})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if k > len(candidates) {
		k = len(candidates)
	}
	results := make([]graph.ScoredNode, k)
	for i := range results {
		results[i] = graph.ScoredNode{
			NodeID:     hex.EncodeToString(candidates[i].id[:]),
			Similarity: candidates[i].score,
		}
	}
	return results, nil
}

func (s *JSONStore) DeleteRecord(_ context.Context, _ string, nodeID [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.nodePath(nodeID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting node file: %w", err)
	}
	return s.removeFromIndex(nodeID)
}

func (s *JSONStore) ListInRange(_ context.Context, _ string, after, before time.Time) ([][16]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.loadIndex()
	if err != nil {
		return nil, err
	}

	var ids [][16]byte
	for _, e := range entries {
		if (after.IsZero() || !e.Timestamp.Before(after)) && e.Timestamp.Before(before) {
			idBytes, err := hex.DecodeString(e.ID)
			if err != nil || len(idBytes) != 16 {
				continue
			}
			var id [16]byte
			copy(id[:], idBytes)
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *JSONStore) ListOlderThan(_ context.Context, _ string, before time.Time) ([][16]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.loadIndex()
	if err != nil {
		return nil, err
	}

	var ids [][16]byte
	for _, e := range entries {
		if e.Timestamp.Before(before) {
			idBytes, err := hex.DecodeString(e.ID)
			if err != nil || len(idBytes) != 16 {
				continue
			}
			var id [16]byte
			copy(id[:], idBytes)
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *JSONStore) ListEntriesByTime(_ context.Context, _ string) ([]StoreEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	raw, err := s.loadIndex()
	if err != nil {
		return nil, err
	}

	entries := make([]StoreEntry, 0, len(raw))
	for _, e := range raw {
		idBytes, err := hex.DecodeString(e.ID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		var id [16]byte
		copy(id[:], idBytes)
		entries = append(entries, StoreEntry{NodeID: id, Timestamp: e.Timestamp})
	}

	// Sort descending by timestamp (most recent first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})
	return entries, nil
}

func (s *JSONStore) Count(_ context.Context, _ string) (int, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.loadIndex()
	if err != nil {
		return 0, time.Time{}, err
	}

	var latest time.Time
	for _, e := range entries {
		if e.Timestamp.After(latest) {
			latest = e.Timestamp
		}
	}
	return len(entries), latest, nil
}

func (s *JSONStore) Close() error { return nil }

// -- helpers ----------------------------------------------------------------

func (s *JSONStore) nodePath(id [16]byte) string {
	return filepath.Join(s.dir, hex.EncodeToString(id[:])+".enc")
}

// indexEncPath returns the path to the encrypted index file.
func (s *JSONStore) indexEncPath() string {
	return filepath.Join(s.dir, "index.enc")
}

// loadIndex reads and decrypts the index file.
// Returns nil (empty index) if the file does not exist yet.
func (s *JSONStore) loadIndex() ([]indexEntry, error) {
	data, err := os.ReadFile(s.indexEncPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading encrypted index: %w", err)
	}

	rec := &EncryptedRecord{
		NodeID:    indexSentinelID,
		Timestamp: indexSentinelTime,
		Data:      data,
	}
	plaintext, err := s.encryptor.Open(rec)
	if err != nil {
		return nil, fmt.Errorf("decrypting index: %w", err)
	}

	var entries []indexEntry
	if err := json.Unmarshal(plaintext, &entries); err != nil {
		return nil, fmt.Errorf("parsing index: %w", err)
	}
	return entries, nil
}

// saveIndex encrypts entries and writes them atomically to index.enc.
func (s *JSONStore) saveIndex(entries []indexEntry) error {
	return s.saveIndexEntries(entries)
}

// saveIndexEntries is the internal implementation shared by saveIndex and
// migrateIndexIfNeeded (the latter runs before s.mu is held).
func (s *JSONStore) saveIndexEntries(entries []indexEntry) error {
	plaintext, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshaling index: %w", err)
	}

	rec, err := s.encryptor.Seal(indexSentinelID, indexSentinelTime, plaintext)
	if err != nil {
		return fmt.Errorf("encrypting index: %w", err)
	}

	// Atomic write: write to .tmp then rename.
	tmp := s.indexEncPath() + ".tmp"
	if err := os.WriteFile(tmp, rec.Data, 0600); err != nil {
		return fmt.Errorf("writing encrypted index: %w", err)
	}
	return os.Rename(tmp, s.indexEncPath())
}

func (s *JSONStore) appendIndex(nodeID [16]byte, ts time.Time) error {
	entries, _ := s.loadIndex()
	id := hex.EncodeToString(nodeID[:])
	for _, e := range entries {
		if e.ID == id {
			return nil // already present
		}
	}
	entries = append(entries, indexEntry{ID: id, Timestamp: ts})
	return s.saveIndex(entries)
}

func (s *JSONStore) removeFromIndex(nodeID [16]byte) error {
	entries, err := s.loadIndex()
	if err != nil {
		return err
	}
	id := hex.EncodeToString(nodeID[:])
	// Use a fresh slice rather than entries[:0] to avoid aliasing: the
	// entries[:0] idiom reuses the same backing array, so range-reads and
	// append-writes would operate on the same memory.
	var filtered []indexEntry
	for _, e := range entries {
		if e.ID != id {
			filtered = append(filtered, e)
		}
	}
	return s.saveIndex(filtered)
}

func (s *JSONStore) timestampFromIndex(nodeID [16]byte) time.Time {
	entries, _ := s.loadIndex()
	id := hex.EncodeToString(nodeID[:])
	for _, e := range entries {
		if e.ID == id {
			return e.Timestamp
		}
	}
	return time.Time{}
}

// cosineSimilarity returns the cosine similarity in [−1, 1] between two vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, magA, magB float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		magA += float64(a[i]) * float64(a[i])
		magB += float64(b[i]) * float64(b[i])
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / (math.Sqrt(magA) * math.Sqrt(magB))
}
