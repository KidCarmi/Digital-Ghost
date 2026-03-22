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
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

	// PERF-1: in-memory embedding cache.
	// All node embeddings are loaded at startup so NearestNeighbors can do
	// cosine similarity in memory (O(N) dot products) rather than O(N) disk
	// decrypts.  For 10k nodes at 768 dims this is ~29 MB.
	embMu    sync.RWMutex
	embCache map[[16]byte][]float32
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
	s := &JSONStore{
		dir:       memoriesDir,
		encryptor: enc,
		logger:    logger,
		embCache:  make(map[[16]byte][]float32),
	}

	// Migrate plaintext index.json → encrypted index.enc if needed.
	if err := s.migrateIndexIfNeeded(); err != nil {
		logger.Warn("index migration failed (will rebuild on next write)", "error", err)
	}

	// ARCH-1: replay WAL to recover any nodes whose index entry was lost
	// due to a crash between WriteRecord and appendIndex.
	if err := s.replayWAL(); err != nil {
		logger.Warn("WAL replay failed (non-fatal)", "error", err)
	}

	// Pre-load all embeddings into the cache so NearestNeighbors is fast.
	s.loadEmbeddingCache()

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

	// ARCH-1: write WAL entry before the .enc file so a crash mid-write
	// leaves a recoverable record.  The WAL is replayed at startup.
	if err := s.appendWAL(nodeID, ts); err != nil {
		// WAL write failure is non-fatal — we proceed, but recovery after
		// a crash between here and appendIndex will be incomplete.
		s.logger.Warn("WAL append failed (non-fatal)", "error", err)
	}

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

	// PERF-1: use in-memory embedding cache — no disk I/O needed.
	// embMu protects embCache; we hold RLock for the full iteration to prevent
	// a concurrent delete from evicting an entry mid-scan.
	s.embMu.RLock()
	defer s.embMu.RUnlock()

	type candidate struct {
		id    [16]byte
		score float64
	}
	candidates := make([]candidate, 0, len(s.embCache))
	for nodeID, emb := range s.embCache {
		sim := cosineSimilarity(query, emb)
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

// updateEmbeddingCache inserts or replaces the cached embedding for nodeID.
// Called by Store.Write via type assertion after a successful WriteRecord.
func (s *JSONStore) updateEmbeddingCache(nodeID [16]byte, emb []float32) {
	if len(emb) == 0 {
		return
	}
	s.embMu.Lock()
	s.embCache[nodeID] = emb
	s.embMu.Unlock()
}

// evictEmbeddingCache removes nodeID from the cache.
// Called by Store.Delete via type assertion after a successful DeleteRecord.
func (s *JSONStore) evictEmbeddingCache(nodeID [16]byte) {
	s.embMu.Lock()
	delete(s.embCache, nodeID)
	s.embMu.Unlock()
}

// loadEmbeddingCache reads every encrypted node file and populates embCache
// with the stored embeddings.  This is called once at startup.
// Non-fatal: nodes that cannot be read or have no embedding are silently skipped.
func (s *JSONStore) loadEmbeddingCache() {
	entries, err := s.loadIndex()
	if err != nil || len(entries) == 0 {
		return
	}

	loaded := 0
	for _, e := range entries {
		idBytes, err := hex.DecodeString(e.ID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		var nodeID [16]byte
		copy(nodeID[:], idBytes)

		data, err := os.ReadFile(s.nodePath(nodeID))
		if err != nil {
			continue
		}
		rec := &EncryptedRecord{NodeID: nodeID, Timestamp: e.Timestamp, Data: data}
		plaintext, err := s.encryptor.Open(rec)
		if err != nil {
			continue
		}
		var node MemoryNode
		if err := json.Unmarshal(plaintext, &node); err != nil || len(node.Embedding) == 0 {
			continue
		}
		s.embCache[nodeID] = node.Embedding
		loaded++
	}

	if loaded > 0 {
		s.logger.Info("embedding cache loaded", "vectors", loaded, "total_nodes", len(entries))
	}
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

// walPath returns the path to the write-ahead log file.
func (s *JSONStore) walPath() string {
	return filepath.Join(s.dir, "wal.log")
}

// appendWAL writes a single line "<hex-nodeID> <unix-nano>\n" to the WAL.
// The file is O_APPEND|O_CREATE so concurrent appends are safe on POSIX
// and Windows (each write is serialised under s.mu which callers hold).
func (s *JSONStore) appendWAL(nodeID [16]byte, ts time.Time) error {
	f, err := os.OpenFile(s.walPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %d\n", hex.EncodeToString(nodeID[:]), ts.UnixNano())
	return err
}

// replayWAL reads wal.log, finds any nodeID whose .enc file exists but is
// absent from the encrypted index, and re-indexes those orphaned nodes.
// After a successful replay the WAL file is removed.
func (s *JSONStore) replayWAL() error {
	walFile := s.walPath()
	f, err := os.Open(walFile)
	if os.IsNotExist(err) {
		return nil // nothing to replay
	}
	if err != nil {
		return fmt.Errorf("opening WAL: %w", err)
	}

	// Parse WAL entries.
	type walEntry struct {
		nodeID [16]byte
		ts     time.Time
	}
	var walEntries []walEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		idBytes, err := hex.DecodeString(parts[0])
		if err != nil || len(idBytes) != 16 {
			continue
		}
		nanos, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		var nodeID [16]byte
		copy(nodeID[:], idBytes)
		walEntries = append(walEntries, walEntry{nodeID: nodeID, ts: time.Unix(0, nanos).UTC()})
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning WAL: %w", err)
	}
	if len(walEntries) == 0 {
		_ = os.Remove(walFile)
		return nil
	}

	// Build a set of nodeIDs already present in the index.
	existing, _ := s.loadIndex()
	inIndex := make(map[string]bool, len(existing))
	for _, e := range existing {
		inIndex[e.ID] = true
	}

	// Re-index any orphan: .enc file exists but not in index.
	recovered := 0
	for _, we := range walEntries {
		hexID := hex.EncodeToString(we.nodeID[:])
		if inIndex[hexID] {
			continue // already indexed
		}
		if _, err := os.Stat(s.nodePath(we.nodeID)); os.IsNotExist(err) {
			continue // .enc file also missing — nothing to recover
		}
		if err := s.appendIndex(we.nodeID, we.ts); err != nil {
			s.logger.Warn("WAL recovery: appendIndex failed", "id", hexID, "error", err)
			continue
		}
		recovered++
		s.logger.Info("WAL recovery: orphaned node re-indexed", "id", hexID)
	}

	// Remove WAL now that replay is complete.
	if err := os.Remove(walFile); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("could not remove WAL after replay", "error", err)
	}
	if recovered > 0 {
		s.logger.Info("WAL replay complete", "recovered", recovered)
	}
	return nil
}

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
