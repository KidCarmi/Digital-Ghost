// Package graph implements the knowledge graph coherence scoring for Digital Ghost.
//
// coherence.go prevents isolated, low-quality memories from polluting the vector DB.
//
// Mechanism:
//   1. After VLM inference produces a description, embed it using a local embedding model.
//   2. Query LanceDB for the K nearest existing memory nodes.
//   3. If the new node's max cosine similarity to existing nodes exceeds the threshold,
//      it is considered "coherent" (connects to the existing knowledge graph).
//   4. Isolated nodes (no connections) require a higher engagement score to be stored.
//
// This prevents "YouTube thumbnail" nodes from accumulating — they have no relationship
// to the user's work knowledge graph and are therefore rejected unless engagement is high.
package graph

import (
	"context"
	"fmt"
	"log/slog"
	"math"
)

// CoherenceScore represents the result of a graph coherence check.
type CoherenceScore struct {
	// MaxSimilarity is the highest cosine similarity found between the new node
	// and any existing node. Range: [-1.0, 1.0].
	MaxSimilarity float64

	// NearestNodeID is the ID of the most similar existing node, or empty if none.
	NearestNodeID string

	// IsCoherent is true if MaxSimilarity >= threshold.
	IsCoherent bool
}

// Checker performs graph coherence checks against a vector store.
// It is safe for concurrent use.
type Checker struct {
	store  VectorReader
	cfg    CoherenceConfig
	logger *slog.Logger
}

// CoherenceConfig holds tunable parameters.
type CoherenceConfig struct {
	// Threshold is the minimum cosine similarity for a node to be "coherent".
	Threshold float64

	// K is the number of nearest neighbors to retrieve.
	K int
}

// VectorReader is the interface the Checker uses to query the vector store.
// This is implemented by storage.LanceDB in production.
type VectorReader interface {
	// NearestNeighbors returns the K most similar nodes to the given embedding.
	// Returns an empty slice if no nodes exist.
	NearestNeighbors(ctx context.Context, embedding []float32, k int) ([]ScoredNode, error)
}

// ScoredNode is a node returned by a nearest-neighbor query.
type ScoredNode struct {
	NodeID     string
	Similarity float64 // Cosine similarity: 1.0 = identical, 0.0 = orthogonal, -1.0 = opposite
}

// NewChecker creates a Checker.
func NewChecker(store VectorReader, cfg CoherenceConfig, logger *slog.Logger) *Checker {
	if cfg.K <= 0 {
		cfg.K = 5
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.4
	}
	return &Checker{store: store, cfg: cfg, logger: logger}
}

// Check computes the coherence score for a new node with the given embedding.
// Returns an error only if the vector store query fails.
func (c *Checker) Check(ctx context.Context, embedding []float32) (CoherenceScore, error) {
	if len(embedding) == 0 {
		return CoherenceScore{IsCoherent: false}, fmt.Errorf("embedding is empty")
	}

	neighbors, err := c.store.NearestNeighbors(ctx, embedding, c.cfg.K)
	if err != nil {
		return CoherenceScore{IsCoherent: false}, fmt.Errorf("querying nearest neighbors: %w", err)
	}

	if len(neighbors) == 0 {
		// No existing nodes — bootstrap case (empty graph).
		// Cannot be incoherent with a graph that doesn't exist yet, so let the
		// normal MinEngagementScore threshold apply rather than IsolatedNodeScore.
		c.logger.Debug("coherence check: graph empty, bootstrap — treating as coherent")
		return CoherenceScore{MaxSimilarity: 0, IsCoherent: true}, nil
	}

	maxSim := -1.0
	nearestID := ""
	for _, n := range neighbors {
		if n.Similarity > maxSim {
			maxSim = n.Similarity
			nearestID = n.NodeID
		}
	}

	isCoherent := maxSim >= c.cfg.Threshold
	c.logger.Debug("coherence check",
		"max_similarity", fmt.Sprintf("%.3f", maxSim),
		"threshold", c.cfg.Threshold,
		"nearest_node", nearestID,
		"coherent", isCoherent)

	return CoherenceScore{
		MaxSimilarity: maxSim,
		NearestNodeID: nearestID,
		IsCoherent:    isCoherent,
	}, nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 for zero-length vectors.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
