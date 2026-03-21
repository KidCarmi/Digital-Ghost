// lancedb.go implements the encrypted vector store interface backed by LanceDB.
//
// Every MemoryNode written to LanceDB is encrypted with AES-256-GCM before storage.
// The key lives in the OS keychain (managed by keychain.go) and never touches disk.
//
// LanceDB is an embedded database — no server process required.
// Data is stored in Apache Arrow format in the configured data directory.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/graph"
)

// MemoryNode is the semantic unit stored in the vector database.
// It represents a single captured screen context that passed all quality filters.
type MemoryNode struct {
	// ID is a unique identifier for this node (UUID v4, stored as [16]byte).
	ID [16]byte

	// CapturedAt is when the source frame was captured.
	CapturedAt time.Time

	// Description is the VLM-generated natural-language description of the content.
	// This is the primary searchable field.
	Description string

	// Tags are keywords extracted from the VLM description.
	Tags []string

	// ProcessName is the application that was active during capture.
	ProcessName string

	// WindowTitle is the title of the active window.
	WindowTitle string

	// BrowserURL is the URL if captured from a browser, else empty.
	BrowserURL string

	// Embedding is the vector representation of the description.
	// Dimension depends on the embedding model (e.g., 768 for nomic-embed-text).
	Embedding []float32

	// EngagementScore is the score computed before storing; retained for analysis.
	EngagementScore float64

	// ViewCount is how many times near-identical content was seen.
	ViewCount int
}

// Store provides read/write access to the encrypted vector store.
type Store struct {
	encryptor *Encryptor
	logger    *slog.Logger
	dataDir   string

	// In production: lancedb.Connect(dataDir) returns a *lancedb.Connection.
	// Using an interface here for testability.
	db lanceDBConn
}

// lanceDBConn is the interface DG uses to interact with LanceDB.
// The production implementation wraps the lancedb-go client.
// A stub implementation is used in tests.
type lanceDBConn interface {
	// WriteRecord writes an encrypted, serialized MemoryNode record.
	WriteRecord(ctx context.Context, table string, nodeID [16]byte, ts time.Time, payload []byte) error

	// ReadRecord reads a raw encrypted record by node ID.
	ReadRecord(ctx context.Context, table string, nodeID [16]byte) ([]byte, time.Time, error)

	// NearestNeighbors returns the K most similar nodes by vector similarity.
	NearestNeighbors(ctx context.Context, table string, embedding []float32, k int) ([]graph.ScoredNode, error)

	// DeleteRecord removes a record from the table.
	DeleteRecord(ctx context.Context, table string, nodeID [16]byte) error

	// ListOlderThan lists node IDs with timestamps before the given time.
	ListOlderThan(ctx context.Context, table string, before time.Time) ([][16]byte, error)

	// Count returns the total number of stored nodes and the timestamp of the
	// most recently captured one (zero Time if no nodes exist).
	Count(ctx context.Context, table string) (int, time.Time, error)

	// Close releases the database connection.
	Close() error
}

const defaultTable = "memories"

// NewStore creates a Store using the given Encryptor and database connection.
func NewStore(encryptor *Encryptor, db lanceDBConn, dataDir string, logger *slog.Logger) *Store {
	return &Store{
		encryptor: encryptor,
		db:        db,
		dataDir:   dataDir,
		logger:    logger,
	}
}

// Write encrypts and stores a MemoryNode.
func (s *Store) Write(ctx context.Context, node *MemoryNode) error {
	payload, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("marshaling node %x: %w", node.ID, err)
	}

	record, err := s.encryptor.Seal(node.ID, node.CapturedAt, payload)
	if err != nil {
		return fmt.Errorf("encrypting node %x: %w", node.ID, err)
	}

	if err := s.db.WriteRecord(ctx, defaultTable, node.ID, node.CapturedAt, record.Data); err != nil {
		return fmt.Errorf("writing node %x to LanceDB: %w", node.ID, err)
	}

	s.logger.Debug("stored memory node",
		"node_id", fmt.Sprintf("%x", node.ID),
		"process", node.ProcessName,
		"tags", node.Tags,
		"engagement", node.EngagementScore)

	return nil
}

// Read retrieves and decrypts a MemoryNode by ID.
// Returns ErrTampered if the integrity check fails.
func (s *Store) Read(ctx context.Context, nodeID [16]byte) (*MemoryNode, error) {
	data, ts, err := s.db.ReadRecord(ctx, defaultTable, nodeID)
	if err != nil {
		return nil, fmt.Errorf("reading node %x from LanceDB: %w", nodeID, err)
	}

	record := &EncryptedRecord{
		NodeID:    nodeID,
		Timestamp: ts,
		Data:      data,
	}

	plaintext, err := s.encryptor.Open(record)
	if err != nil {
		// ErrTampered is returned as-is; the caller should quarantine this node.
		return nil, err
	}

	var node MemoryNode
	if err := json.Unmarshal(plaintext, &node); err != nil {
		return nil, fmt.Errorf("unmarshaling node %x: %w", nodeID, err)
	}

	return &node, nil
}

// NearestNeighbors queries the store for the K most similar nodes to the given embedding.
// Implements graph.VectorReader.
func (s *Store) NearestNeighbors(ctx context.Context, embedding []float32, k int) ([]graph.ScoredNode, error) {
	return s.db.NearestNeighbors(ctx, defaultTable, embedding, k)
}

// Delete removes a single node from the store.
// Uses secure deletion if configured.
func (s *Store) Delete(ctx context.Context, nodeID [16]byte) error {
	if err := s.db.DeleteRecord(ctx, defaultTable, nodeID); err != nil {
		return fmt.Errorf("deleting node %x: %w", nodeID, err)
	}
	s.logger.Info("deleted memory node", "node_id", fmt.Sprintf("%x", nodeID))
	return nil
}

// Stats returns the total node count and the timestamp of the most recent capture.
func (s *Store) Stats(ctx context.Context) (count int, lastCapture time.Time, err error) {
	return s.db.Count(ctx, defaultTable)
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
