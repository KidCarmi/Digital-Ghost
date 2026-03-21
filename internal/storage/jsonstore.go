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

// NewJSONStore creates a JSONStore rooted at dir/memories/.
// The directory is created if it does not exist.
func NewJSONStore(dir string, enc *Encryptor, logger *slog.Logger) (*JSONStore, error) {
	memoriesDir := filepath.Join(dir, "memories")
	if err := os.MkdirAll(memoriesDir, 0700); err != nil {
		return nil, fmt.Errorf("creating memories directory: %w", err)
	}
	return &JSONStore{dir: memoriesDir, encryptor: enc, logger: logger}, nil
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
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(query) == 0 {
		return nil, nil
	}

	entries, err := s.loadIndex()
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

func (s *JSONStore) Close() error { return nil }

// -- helpers ----------------------------------------------------------------

func (s *JSONStore) nodePath(id [16]byte) string {
	return filepath.Join(s.dir, hex.EncodeToString(id[:])+".enc")
}

func (s *JSONStore) indexPath() string {
	return filepath.Join(s.dir, "index.json")
}

func (s *JSONStore) loadIndex() ([]indexEntry, error) {
	data, err := os.ReadFile(s.indexPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *JSONStore) saveIndex(entries []indexEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
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
	filtered := entries[:0]
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
