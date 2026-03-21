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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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
	store     *storage.Store
	client    *inference.Client
	model     string
	logger    *slog.Logger
	srv       *http.Server
	csrfToken string // random token generated at startup; required on mutating endpoints
	uiHTML    []byte // ui.html with __CSRF_TOKEN__ substituted
}

// New creates a Server. Call Run() to start listening.
// Returns an error if the OS CSPRNG is unavailable (effectively impossible
// on any supported platform, but handled explicitly rather than via panic).
func New(store *storage.Store, client *inference.Client, model string, logger *slog.Logger) (*Server, error) {
	// Generate a random CSRF token for this server lifetime.
	// Any request to a mutating endpoint must supply this token as
	// X-DG-CSRF-Token. Cross-origin pages cannot read the token from the UI
	// (CORS), so they cannot forge valid mutating requests.
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, fmt.Errorf("CSRF token generation failed: %w", err)
	}
	csrfToken := hex.EncodeToString(tokenBytes[:])

	s := &Server{
		store:     store,
		client:    client,
		model:     model,
		logger:    logger,
		csrfToken: csrfToken,
		uiHTML:    bytes.ReplaceAll(uiHTML, []byte("__CSRF_TOKEN__"), []byte(csrfToken)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/status", s.handleStatus)

	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/delete", s.handleDelete)
	mux.HandleFunc("/api/timeline", s.handleTimeline)

	s.srv = &http.Server{
		Addr:         defaultAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 150 * time.Second, // chat endpoint can take up to ~90s for LLM cold-start
	}
	return s, nil
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
	w.Write(s.uiHTML)
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
		if ctx.Err() != nil {
			s.logger.Debug("query: client disconnected during embed")
			return
		}
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

	// minQuerySimilarity is the minimum cosine similarity for a result to be
	// returned. Results below this are not meaningfully related to the query.
	// nomic-embed-text cosine space: 0.3 is a reasonable "related" threshold.
	const minQuerySimilarity = 0.3

	var results []QueryResult
	for _, sn := range scored {
		if sn.Similarity < minQuerySimilarity {
			continue // skip low-relevance noise
		}
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
		if ctx.Err() != nil {
			// Client disconnected before we finished — not an error worth logging.
			s.logger.Debug("chat: client disconnected during embed")
			return
		}
		s.logger.Warn("chat: embed failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed: " + err.Error()})
		return
	}

	scored, err := s.store.NearestNeighbors(ctx, embedding, 8)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed: " + err.Error()})
		return
	}

	// minChatSimilarity is the floor for showing a memory as a source card.
	// minChatContextSimilarity is the higher bar for including a memory in the
	// LLM context and for deciding whether the question is a memory lookup at all.
	// 0.65 filters out incidental matches (e.g. greetings that happen to score
	// 0.50–0.60 against stored memories just because the embedding space has a
	// non-zero baseline).
	const (
		minChatSimilarity        = 0.3
		minChatContextSimilarity = 0.65
	)

	var sources []QueryResult
	var sb strings.Builder // context fed to LLM — only high-similarity memories
	ctxIdx := 0
	for _, sn := range scored {
		// Always collect sources for the UI cards at the lower threshold.
		if sn.Similarity >= minChatSimilarity {
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

			// Only include in LLM context if it clears the higher bar.
			if sn.Similarity >= minChatContextSimilarity {
				ctxIdx++
				// Plain prose format — no [N] numbering or pipe-separated metadata
				// that the model tends to copy verbatim into its answer.
				fmt.Fprintf(&sb, "Memory %d (%s in %s): %s\n\n",
					ctxIdx,
					node.CapturedAt.Format("3:04 PM"),
					node.ProcessName,
					node.Description,
				)
			}
		}
	}

	// topSimilarity is the highest score among all results (first element,
	// since NearestNeighbors returns descending order).
	topSimilarity := 0.0
	if len(scored) > 0 {
		topSimilarity = scored[0].Similarity
	}

	var answer string
	switch {
	case len(sources) == 0:
		answer = "I don't have anything relevant stored yet — keep me running and I'll start building up your memory. Try asking again in a bit!"

	case topSimilarity < minChatContextSimilarity:
		// Conversational / casual query — no memory context injected.
		// Greetings and chitchat often score 0.50–0.62 against random stored
		// memories; injecting that context just causes the model to narrate
		// screen content instead of answering the question.
		prompt := "You are Digital Ghost, a friendly personal memory assistant.\n" +
			"The user said: \"" + q + "\"\n\n" +
			"Reply in one or two short, natural sentences. " +
			"Do NOT summarise screen content or describe what you have seen. " +
			"If it is a greeting, just greet back warmly and briefly."
		answer, err = s.client.Generate(ctx, prompt)
		if err != nil {
			if ctx.Err() != nil {
				s.logger.Debug("chat: client disconnected during LLM generation")
				return
			}
			s.logger.Warn("chat: LLM generation failed", "error", err)
			answer = ""
		}

	default:
		// Memory-anchored query — ground the answer in the retrieved context.
		prompt := "You are Digital Ghost, a personal memory assistant.\n" +
			"The user is asking: \"" + q + "\"\n\n" +
			"Background — recent activity that seems relevant:\n\n" +
			sb.String() +
			"Answer in 1-3 short sentences using your own words. " +
			"IMPORTANT: do NOT copy or quote any of the background text above — synthesise a natural answer from it. " +
			"Do not mention screenshots, screen captures, or memories. " +
			"If the background does not answer the question well, say so briefly."
		answer, err = s.client.Generate(ctx, prompt)
		if err != nil {
			if ctx.Err() != nil {
				s.logger.Debug("chat: client disconnected during LLM generation")
				return
			}
			s.logger.Warn("chat: LLM generation failed", "error", err)
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

	// CSRF check: any page the user browses to can POST to localhost:7327 via
	// a form submission (no CORS preflight). The custom header cannot be added
	// by a cross-origin form, only by our own UI (same-origin fetch/XHR).
	if r.Header.Get("X-DG-CSRF-Token") != s.csrfToken {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or missing CSRF token"})
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

// handleTimeline returns memory nodes ordered by capture time descending.
//
// GET /api/timeline?limit=50&offset=0
// Response: {"entries":[...],"total":N,"limit":50,"offset":0}
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	limit := 50
	offset := 0

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	entries, total, err := s.store.Timeline(r.Context(), limit, offset)
	if err != nil {
		s.logger.Warn("timeline query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "timeline failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
