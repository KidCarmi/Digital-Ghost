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
	"strings"
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

	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/delete", s.handleDelete)

	s.srv = &http.Server{
		Addr:         defaultAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 150 * time.Second, // chat endpoint can take up to ~90s for LLM cold-start
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
	count, lastCapture, err := s.store.Stats(r.Context())
	if err != nil {
		s.logger.Warn("stats query failed", "error", err)
	}
	resp := StatusResponse{
		Nodes:       count,
		Model:       s.model,
		LastCapture: lastCapture,
	}
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

// handleChat retrieves relevant memories and returns a conversational answer
// synthesised by the local LLM, along with the source memory cards.
//
// GET /api/chat?q=<question>
// Response: {"answer":"...","sources":[...],"query":"..."}
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing query parameter q"})
		return
	}

	// Allow up to 120s for embed + neighbour search + LLM generation.
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	embedding, err := s.client.Embed(ctx, q)
	if err != nil {
		s.logger.Warn("chat: embed failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed: " + err.Error()})
		return
	}

	scored, err := s.store.NearestNeighbors(ctx, embedding, 8)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed: " + err.Error()})
		return
	}

	var sources []QueryResult
	var sb strings.Builder
	for i, sn := range scored {
		idBytes, err := hex.DecodeString(sn.NodeID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		var nodeID [16]byte
		copy(nodeID[:], idBytes)
		node, err := s.store.Read(ctx, nodeID)
		if err != nil {
			continue
		}
		sources = append(sources, QueryResult{
			ID:          sn.NodeID,
			CapturedAt:  node.CapturedAt,
			Description: node.Description,
			App:         node.ProcessName,
			WindowTitle: node.WindowTitle,
			BrowserURL:  node.BrowserURL,
			Tags:        node.Tags,
			Similarity:  sn.Similarity,
		})
		fmt.Fprintf(&sb, "[%d] %s | %s | %s\n%s\n\n",
			i+1,
			node.CapturedAt.Format("Jan 2 3:04 PM"),
			node.ProcessName,
			node.WindowTitle,
			node.Description,
		)
	}

	var answer string
	if sb.Len() == 0 {
		answer = "I don't have anything relevant stored yet — keep me running and I'll start building up your memory. Try asking again in a bit!"
	} else {
		prompt := "You are Digital Ghost, a warm personal assistant who has been quietly watching over the user's screen to help them remember their day.\n" +
			"The user is asking: \"" + q + "\"\n\n" +
			"Here are some moments from their screen that seem relevant:\n\n" +
			sb.String() +
			"Respond naturally and warmly in 2-4 sentences, like a thoughtful friend who remembers things for them. " +
			"Don't mention screenshots, memories, or snapshots — just speak as if you were there. " +
			"If what you saw doesn't fully answer the question, be honest about it in a friendly way."

		answer, err = s.client.Generate(ctx, prompt)
		if err != nil {
			s.logger.Warn("chat: LLM generation failed", "error", err)
			// Degrade gracefully — return sources without an answer.
			answer = ""
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"answer":  answer,
		"sources": sources,
		"query":   q,
	})
}

// handleDelete deletes memory nodes in a given time range.
//
// POST /api/delete?range=today|week|month|all
// Response: {"deleted": N, "range": "..."}
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}

	rangeParam := r.URL.Query().Get("range")
	now := time.Now()
	var after time.Time

	switch rangeParam {
	case "today":
		after = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "week":
		after = now.AddDate(0, 0, -7)
	case "month":
		after = now.AddDate(0, 0, -30)
	case "all":
		// zero after = from the beginning of time
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "range must be one of: today, week, month, all",
		})
		return
	}

	deleted, err := s.store.DeleteRange(r.Context(), after, now.Add(time.Second))
	if err != nil {
		s.logger.Warn("delete range partially failed", "range", rangeParam, "deleted", deleted, "error", err)
	}

	s.logger.Info("user-initiated memory deletion", "range", rangeParam, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "range": rangeParam})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
