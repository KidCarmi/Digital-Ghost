// Digital Ghost — Main Entry Point
//
// Startup sequence (order is a safety constraint, not a preference):
//  1. Refuse to run as root.
//  2. Load and validate configuration.
//  3. Initialize OS keychain and encryption.
//  4. Check consent record; if absent, run consent flow.
//  5. Start system tray icon (fatal if tray unavailable).
//  6. Verify Ollama is reachable (warn if not; don't fail — Ollama may start later).
//  7. Start capture, inference, and storage goroutines.
//  8. Block until SIGINT/SIGTERM or tray "Stop" action.
//  9. Graceful shutdown: drain queues, flush writes, zero key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/capture"
	"github.com/KidCarmi/digital-ghost/internal/config"
	"github.com/KidCarmi/digital-ghost/internal/filter"
	"github.com/KidCarmi/digital-ghost/internal/graph"
	"github.com/KidCarmi/digital-ghost/internal/inference"
	"github.com/KidCarmi/digital-ghost/internal/storage"
	"github.com/KidCarmi/digital-ghost/internal/tray"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "digitalghost: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── CLI flags ──────────────────────────────────────────────────────────────
	var (
		configPath = flag.String("config", "~/.config/digitalghost/config.yaml", "Path to config file")
		wipe       = flag.Bool("wipe", false, "Wipe all stored data (requires --confirm)")
		confirm    = flag.Bool("confirm", false, "Required with --wipe")
		statusCmd  = flag.Bool("status", false, "Show current daemon status and exit")
	)
	flag.Parse()

	// ── Step 1: Refuse to run as root ─────────────────────────────────────────
	if os.Getuid() == 0 {
		return fmt.Errorf("Digital Ghost must not run as root. " +
			"Running as root removes OS-level user isolation and makes every safety " +
			"property in this system meaningless. Exiting.")
	}

	// ── Logging ───────────────────────────────────────────────────────────────
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Step 2: Load configuration ────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Create data directory.
	if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
		return fmt.Errorf("creating data directory %q: %w", cfg.Storage.DataDir, err)
	}

	// ── Step 3: Encryption and keychain ──────────────────────────────────────
	km, err := storage.NewKeyManager()
	if err != nil {
		return fmt.Errorf("keychain initialization failed: %w\n\n"+
			"Digital Ghost requires OS keychain access to protect your data.\n"+
			"Ensure gnome-keyring (Linux), Keychain (macOS), or DPAPI (Windows) is available.", err)
	}
	defer km.Close()

	encryptor, err := storage.NewEncryptor(km.Key())
	if err != nil {
		return fmt.Errorf("initializing encryptor: %w", err)
	}

	// ── Wipe command ──────────────────────────────────────────────────────────
	if *wipe {
		if !*confirm {
			return fmt.Errorf("--wipe requires --confirm. This operation is irreversible.\n" +
				"Run: digitalghost --wipe --confirm")
		}
		return runWipe(cfg, encryptor, km, logger)
	}

	// ── Status command ────────────────────────────────────────────────────────
	if *statusCmd {
		return runStatus(cfg, logger)
	}

	// ── Step 4: Consent check ─────────────────────────────────────────────────
	consentMgr := tray.New(cfg.Storage.DataDir, version)
	if !consentMgr.IsConsentGranted() {
		logger.Info("no consent record found; starting consent flow")
		if err := consentMgr.RequestConsent(); err != nil {
			return fmt.Errorf("consent required to start: %w", err)
		}
		logger.Info("consent granted; starting Digital Ghost")
	}

	// ── Step 5: System tray icon ──────────────────────────────────────────────
	stopCh := make(chan struct{})
	if err := tray.StartTrayIcon(func() {
		logger.Info("stop requested via tray icon")
		close(stopCh)
	}); err != nil {
		return fmt.Errorf("tray icon failed to start: %w\n\n"+
			"Digital Ghost requires a visible tray icon to maintain transparency.\n"+
			"There is no headless/background mode.\n"+
			"If you're running a headless server, Digital Ghost is not the right tool.", err)
	}

	// ── Step 6: Verify Ollama ─────────────────────────────────────────────────
	ollamaClient := inference.NewClient(cfg.Inference, logger)
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := ollamaClient.Ping(pingCtx); err != nil {
		// Non-fatal: Ollama may not be running yet. Inference will fail until it is.
		logger.Warn("Ollama not reachable at startup; inference will be unavailable until Ollama starts",
			"url", cfg.Inference.OllamaURL, "model", cfg.Inference.Model)
	} else {
		logger.Info("Ollama connected", "url", cfg.Inference.OllamaURL, "model", cfg.Inference.Model)
	}

	// ── Step 7: Initialize components ────────────────────────────────────────
	blocklistPath := filepath.Join(os.Getenv("HOME"), ".config/digitalghost/blocklist.yaml")
	blocklist, err := filter.NewBlocklist(blocklistPath, logger)
	if err != nil {
		return fmt.Errorf("initializing blocklist: %w", err)
	}
	defer blocklist.Close()

	gate := capture.NewGate(blocklist, logger)
	frameQueue := inference.NewQueue(cfg.Capture.QueueDepth, logger)
	governor := inference.NewGovernor(cfg.ResourceBudget, logger)
	defer governor.Close()

	// Storage setup (using stub DB for architecture scaffold).
	var db stubLanceDB
	store := storage.NewStore(encryptor, &db, cfg.Storage.DataDir, logger)
	defer store.Close()

	coherenceChecker := graph.NewChecker(store, graph.CoherenceConfig{
		Threshold: cfg.SemanticFilter.GraphCoherenceThreshold,
		K:         5,
	}, logger)

	retentionMgr := storage.NewRetentionManager(
		store, km,
		cfg.Storage.DataDir,
		cfg.Storage.RetentionDays,
		cfg.Storage.SecureDelete,
		logger,
	)

	// ── Step 8: Start goroutines ──────────────────────────────────────────────
	logger.Info("Digital Ghost starting",
		"version", version,
		"data_dir", cfg.Storage.DataDir,
		"model", cfg.Inference.Model,
		"fps", cfg.Capture.FPS,
		"max_cpu_pct", cfg.ResourceBudget.MaxCPUPct)

	go runCaptureLoop(cfg, gate, frameQueue, stopCh, logger)
	go runInferenceLoop(cfg, governor, ollamaClient, frameQueue, coherenceChecker, store, stopCh, logger)
	go retentionMgr.RunScheduled(stopCh)

	// ── Step 9: Block until shutdown ─────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("received signal; shutting down", "signal", sig)
		close(stopCh)
	case <-stopCh:
		logger.Info("stop channel closed; shutting down")
	}

	// Give goroutines 10 seconds to drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = shutdownCtx

	logger.Info("Digital Ghost stopped")
	return nil
}

// runCaptureLoop is the capture goroutine.
func runCaptureLoop(cfg *config.Config, gate *capture.Gate, q *inference.Queue, stopCh <-chan struct{}, logger *slog.Logger) {
	capturer, err := capture.NewX11Capturer(cfg, gate, makeChan(q), logger)
	if err != nil {
		logger.Error("failed to initialize X11 capturer", "error", err)
		return
	}
	defer capturer.Close()
	if err := capturer.Run(stopCh); err != nil {
		logger.Error("capture loop exited with error", "error", err)
	}
}

// runInferenceLoop is the inference goroutine.
func runInferenceLoop(
	cfg *config.Config,
	gov *inference.Governor,
	client *inference.Client,
	q *inference.Queue,
	coherence *graph.Checker,
	store *storage.Store,
	stopCh <-chan struct{},
	logger *slog.Logger,
) {
	for {
		frame := q.Pop(stopCh)
		if frame == nil {
			return // stopCh closed.
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Inference.TimeoutSec)*time.Second)

		// Wait for resource budget.
		if err := gov.Acquire(ctx); err != nil {
			cancel()
			if err == context.Canceled || err == context.DeadlineExceeded {
				return
			}
			logger.Warn("governor acquire error", "error", err)
			continue
		}

		// Run inference.
		result, err := client.Infer(ctx, frame)
		cancel()
		if err != nil {
			logger.Warn("inference failed", "error", err)
			continue
		}

		// Compute engagement score.
		signals := filter.EngagementSignals{
			DwellSeconds: 5, // TODO: wire real dwell time from capture.
			ContentClass: filter.Classify(filter.ClassifierInput{
				ProcessName: frame.WindowCtx.ProcessName,
				WindowTitle: frame.WindowCtx.WindowTitle,
				BrowserURL:  frame.WindowCtx.BrowserURL,
			}),
			ViewCount:  1,
			CapturedAt: frame.CapturedAt,
		}
		score := filter.EngagementScore(signals)

		if score < cfg.SemanticFilter.MinEngagementScore {
			logger.Debug("frame discarded: low engagement", "score", score, "threshold", cfg.SemanticFilter.MinEngagementScore)
			continue
		}

		// TODO: Compute embedding via Ollama nomic-embed-text API, then coherence check.
		// For now: stub embedding for architecture scaffold.
		var embedding []float32

		coherenceScore, err := coherence.Check(context.Background(), embedding)
		if err != nil {
			logger.Debug("coherence check failed (non-fatal)", "error", err)
		}

		requiredScore := cfg.SemanticFilter.MinEngagementScore
		if !coherenceScore.IsCoherent {
			requiredScore = cfg.SemanticFilter.IsolatedNodeScore
		}
		if score < requiredScore {
			logger.Debug("frame discarded: isolated node with insufficient engagement",
				"score", score, "required", requiredScore)
			continue
		}

		// Store the node.
		var nodeID [16]byte
		// TODO: generate proper UUID v4.
		node := &storage.MemoryNode{
			ID:              nodeID,
			CapturedAt:      frame.CapturedAt,
			Description:     result.Description,
			Tags:            result.Tags,
			ProcessName:     frame.WindowCtx.ProcessName,
			WindowTitle:     frame.WindowCtx.WindowTitle,
			BrowserURL:      frame.WindowCtx.BrowserURL,
			Embedding:       embedding,
			EngagementScore: score,
			ViewCount:       1,
		}
		if err := store.Write(context.Background(), node); err != nil {
			logger.Warn("failed to store memory node", "error", err)
		}
	}
}

func runWipe(cfg *config.Config, enc *storage.Encryptor, km *storage.KeyManager, logger *slog.Logger) error {
	logger.Info("WIPE: starting permanent data destruction")
	var db stubLanceDB
	store := storage.NewStore(enc, &db, cfg.Storage.DataDir, logger)
	rm := storage.NewRetentionManager(store, km, cfg.Storage.DataDir, cfg.Storage.RetentionDays, cfg.Storage.SecureDelete, logger)
	return rm.WipeAll(context.Background())
}

func runStatus(cfg *config.Config, logger *slog.Logger) error {
	fmt.Printf("Digital Ghost v%s\n", version)
	fmt.Printf("Data directory: %s\n", cfg.Storage.DataDir)
	fmt.Printf("Model: %s\n", cfg.Inference.Model)
	fmt.Printf("Retention: %d days\n", cfg.Storage.RetentionDays)
	fmt.Printf("Max CPU: %d%%  Max GPU: %d%%\n", cfg.ResourceBudget.MaxCPUPct, cfg.ResourceBudget.MaxGPUPct)
	return nil
}

// makeChan adapts a *inference.Queue to a chan<- *capture.Frame for the capturer.
// The capturer pushes directly to the channel; the queue wrapper handles overflow.
func makeChan(q *inference.Queue) chan<- *capture.Frame {
	ch := make(chan *capture.Frame, 1)
	go func() {
		for f := range ch {
			q.Push(f)
		}
	}()
	return ch
}

// stubLanceDB is a no-op implementation of storage.lanceDBConn for the architecture scaffold.
// Replace with the real LanceDB client in production.
type stubLanceDB struct{}

func (s *stubLanceDB) WriteRecord(_ context.Context, _ string, _ [16]byte, _ time.Time, _ []byte) error {
	return nil
}
func (s *stubLanceDB) ReadRecord(_ context.Context, _ string, _ [16]byte) ([]byte, time.Time, error) {
	return nil, time.Time{}, nil
}
func (s *stubLanceDB) NearestNeighbors(_ context.Context, _ string, _ []float32, _ int) ([]graph.ScoredNode, error) {
	return nil, nil
}
func (s *stubLanceDB) DeleteRecord(_ context.Context, _ string, _ [16]byte) error { return nil }
func (s *stubLanceDB) ListOlderThan(_ context.Context, _ string, _ time.Time) ([][16]byte, error) {
	return nil, nil
}
func (s *stubLanceDB) Close() error { return nil }
