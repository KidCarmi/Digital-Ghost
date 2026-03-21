// Package api provides the local HTTP search interface for Digital Ghost.
//
// The server runs at http://localhost:7327 and exposes:
//
//	GET /              — single-page search UI (embedded HTML)
//	GET /api/query?q=  — semantic search, returns JSON results
//	GET /api/status    — daemon health and node count
//
// The server binds only to 127.0.0.1 (loopback) — it is never reachable
// from other machines.
package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
	_ "embed"

	"github.com/KidCarmi/digital-ghost/internal/inference"
	"github.com/KidCarmi/digital-ghost/internal/storage"
)

//go:embed ui.html
var uiHTML []byte

const defaultAddr = "127.0.0.1:7327"

// QueryResult is a single search result returned to the UI.
type QueryResult struct {
	ID          string    `json:"id"`
	CapturedAt  time.Time `json:"captured_at"`
	Description string    `json:"description"`
	App         string    `json:"app"`
	WindowTitle string    `json:"window_title"`
	BrowserURL  string    `json:"browser_url,omitempty"`
	Tags        []string  `json:"tags"`
	Similarity  float64   `json:"similarity"`
}

// StatusResponse is returned by /api/status.
type StatusResponse struct {
	Nodes       int       `json:"nodes"`
	Model       string    `json:"model"`
	LastCapture time.Time `json:"last_capture,omitempty"`
}

// Server is the Digital Ghost local HTTP server.
type Server struct {
	store  *storage.Store
	client *inference.Client
	model  string
	logger *slog.Logger
	srv    *http.Server
}

// New creates a Server. Call Run() to start listening.
func New(store *storage.Store, client *inference.Client, model string, logger *slog.Logger) *Server {
	s := &Server{store: store, client: client, model: model, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/status", s.handleStatus)

	s.srv = &http.Server{
		Addr:         defaultAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	return s
}

// Run starts the HTTP server. Blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", defaultAddr)
	if err != nil {
		return fmt.Errorf("binding API server on %s: %w", defaultAddr, err)
	}
	s.logger.Info("API server listening", "url", "http://"+defaultAddr)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// -- handlers ---------------------------------------------------------------

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{Model: s.model}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing query parameter q"})
		return
	}

	// Embed the query text.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	embedding, err := s.client.Embed(ctx, q)
	if err != nil {
		s.logger.Warn("embed failed during query", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed: " + err.Error()})
		return
	}

	// Find nearest neighbours.
	scored, err := s.store.NearestNeighbors(ctx, embedding, 8)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed: " + err.Error()})
		return
	}

	var results []QueryResult
	for _, sn := range scored {
		idBytes, err := hex.DecodeString(sn.NodeID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		var nodeID [16]byte
		copy(nodeID[:], idBytes)

		node, err := s.store.Read(ctx, nodeID)
		if err != nil {
			s.logger.Debug("failed to read node during query", "id", sn.NodeID, "error", err)
			continue
		}

		results = append(results, QueryResult{
			ID:          sn.NodeID,
			CapturedAt:  node.CapturedAt,
			Description: node.Description,
			App:         node.ProcessName,
			WindowTitle: node.WindowTitle,
			BrowserURL:  node.BrowserURL,
			Tags:        node.Tags,
			Similarity:  sn.Similarity,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results, "query": q})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
