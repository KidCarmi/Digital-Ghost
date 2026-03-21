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
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/api"
	"github.com/KidCarmi/digital-ghost/internal/capture"
	"github.com/KidCarmi/digital-ghost/internal/config"
	"github.com/KidCarmi/digital-ghost/internal/defaults"
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

	// ── Bootstrap logger (before config load) ────────────────────────────────
	// Replaced below once config is loaded and the log file path is known.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)
	// In Go 1.21+, slog.SetDefault also bridges the legacy log package to slog
	// at INFO level. fyne.io/systray uses log.Println() internally and emits
	// benign Windows quirk messages (e.g. "The operation completed successfully.")
	// that pollute our structured log. Silence the legacy logger — we use slog
	// exclusively for our own logging.
	log.SetOutput(io.Discard)

	// ── Step 2: Load configuration ────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// ── Logging (re-initialise from config) ───────────────────────────────────
	{
		level := slog.LevelInfo
		if cfg.Logging.Level == "debug" {
			level = slog.LevelDebug
		}
		var logWriter io.Writer = os.Stdout
		if cfg.Logging.File != "" {
			if err := os.MkdirAll(filepath.Dir(cfg.Logging.File), 0700); err == nil {
				if f, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
					logWriter = io.MultiWriter(os.Stdout, f)
					// f is intentionally not closed here — it lives for the daemon lifetime.
					// The OS closes it on process exit.
				}
			}
		}
		logger = slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: level}))
		slog.SetDefault(logger)
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
	// gate is created after the blocklist below, but we need stopCh now.
	// Pause/resume callbacks are wired to gate.Pause()/gate.Resume() below.
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(func() { close(stopCh) }) }

	var pauseCallback, resumeCallback func()
	if err := tray.StartTrayIcon(
		func() { // onStop
			logger.Info("stop requested via tray icon")
			stopFn()
		},
		func() { // onPause — forward to gate once it's initialised
			if pauseCallback != nil {
				pauseCallback()
			}
		},
		func() { // onResume — forward to gate once it's initialised
			if resumeCallback != nil {
				resumeCallback()
			}
		},
	); err != nil {
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
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	blocklistPath := filepath.Join(homeDir, ".config", "digitalghost", "blocklist.yaml")
	if err := ensureDefaultBlocklist(blocklistPath); err != nil {
		logger.Warn("could not create default blocklist", "error", err)
	}
	blocklist, err := filter.NewBlocklist(blocklistPath, logger)
	if err != nil {
		return fmt.Errorf("initializing blocklist: %w", err)
	}
	defer blocklist.Close()

	gate := capture.NewGate(blocklist, logger)

	// Wire tray pause/resume callbacks to the gate now that it exists.
	pauseCallback = func() {
		gate.Pause()
		logger.Info("capture paused by user")
	}
	resumeCallback = func() {
		gate.Resume()
		logger.Info("capture resumed by user")
	}

	frameQueue := inference.NewQueue(cfg.Capture.QueueDepth, logger)
	governor := inference.NewGovernor(cfg.ResourceBudget, logger)
	defer governor.Close()

	// Storage setup — flat JSON file store (one encrypted file per node).
	db, err := storage.NewJSONStore(cfg.Storage.DataDir, encryptor, logger)
	if err != nil {
		return fmt.Errorf("initializing storage: %w", err)
	}
	store := storage.NewStore(encryptor, db, cfg.Storage.DataDir, logger)
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

	// Start the local web search UI.
	apiCtx, apiCancel := context.WithCancel(context.Background())
	defer apiCancel()
	go func() {
		<-stopCh
		apiCancel()
	}()
	apiServer := api.New(store, ollamaClient, cfg.Inference.Model, logger)
	go func() {
		if err := apiServer.Run(apiCtx); err != nil {
			logger.Warn("API server stopped", "error", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("capture goroutine panicked — stopping daemon",
					"panic", r)
				stopFn() // close stopCh so the tray icon can show an error state
			}
		}()
		runCaptureLoop(cfg, gate, frameQueue, stopCh, logger)
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("inference goroutine panicked — stopping daemon",
					"panic", r)
				stopFn()
			}
		}()
		runInferenceLoop(cfg, governor, ollamaClient, frameQueue, coherenceChecker, store, stopCh, logger)
	}()
	go retentionMgr.RunScheduled(stopCh)

	// ── Step 9: Block until shutdown ─────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("received signal; shutting down", "signal", sig)
		stopFn()
	case <-stopCh:
		logger.Info("stop channel closed; shutting down")
	}

	// Give goroutines 10 seconds to drain before hard exit.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
		logger.Info("all goroutines stopped cleanly")
	case <-shutdownCtx.Done():
		logger.Warn("shutdown timed out waiting for goroutines to drain")
	}

	logger.Info("Digital Ghost stopped")
	return nil
}

// runCaptureLoop is the capture goroutine.
func runCaptureLoop(cfg *config.Config, gate *capture.Gate, q *inference.Queue, stopCh <-chan struct{}, logger *slog.Logger) {
	// Bridge: capturer writes to ch; the goroutine below pushes into the queue.
	// ch is closed after the capturer exits so the bridge goroutine terminates cleanly.
	ch := make(chan *capture.Frame, 1)
	go func() {
		for f := range ch {
			q.Push(f)
		}
	}()

	capturer, err := capture.NewCapturer(cfg, gate, ch, logger)
	if err != nil {
		logger.Error("failed to initialize capturer", "error", err)
		close(ch)
		return
	}
	defer func() {
		capturer.Close()
		close(ch) // signals the bridge goroutine to exit
	}()
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

		// Hard dwell threshold — skip frames where the window was active for less
		// than MinDwellSeconds. This avoids spending GPU time on transient glances
		// (e.g. alt-tab flashes) before engagement scoring runs.
		if frame.DwellSeconds < cfg.SemanticFilter.MinDwellSeconds {
			cancel()
			logger.Debug("frame skipped: insufficient dwell",
				"dwell_sec", frame.DwellSeconds,
				"min", cfg.SemanticFilter.MinDwellSeconds)
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
		// TypedWithinSeconds uses the single available input timestamp.
		// ScrolledWithinSeconds and ClickedWithinSeconds are left 0 because
		// GetLastInputInfo does not distinguish event types — using the same value
		// for all three would create phantom signals and inflate the score.
		signals := filter.EngagementSignals{
			DwellSeconds:          frame.DwellSeconds,
			TypedWithinSeconds:    frame.SecondsSinceInput,
			ScrolledWithinSeconds: 0,
			ClickedWithinSeconds:  0,
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

		// Compute embedding for semantic search and coherence check.
		// Enrich the embed text with window metadata so that searches for
		// app names, window titles, and URLs match stored nodes even when the
		// VLM description doesn't explicitly repeat that information.
		embedText := result.Description
		if frame.WindowCtx.ProcessName != "" {
			embedText += "\nApp: " + frame.WindowCtx.ProcessName
		}
		if frame.WindowCtx.WindowTitle != "" {
			embedText += "\nWindow: " + frame.WindowCtx.WindowTitle
		}
		if frame.WindowCtx.BrowserURL != "" {
			embedText += "\nURL: " + frame.WindowCtx.BrowserURL
		}
		if len(result.Tags) > 0 {
			embedText += "\nTags: " + strings.Join(result.Tags, ", ")
		}

		embedCtx, embedCancel := context.WithTimeout(context.Background(), time.Duration(cfg.Inference.TimeoutSec)*time.Second)
		embedding, embedErr := client.Embed(embedCtx, embedText)
		embedCancel()
		if embedErr != nil {
			logger.Debug("embedding failed (non-fatal); storing without vector", "error", embedErr)
		}

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
		if _, err := io.ReadFull(rand.Reader, nodeID[:]); err != nil {
			logger.Warn("UUID generation failed", "error", err)
			continue
		}
		nodeID[6] = (nodeID[6] & 0x0f) | 0x40 // version 4
		nodeID[8] = (nodeID[8] & 0x3f) | 0x80 // variant bits
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
	db, err := storage.NewJSONStore(cfg.Storage.DataDir, enc, logger)
	if err != nil {
		return fmt.Errorf("opening storage for wipe: %w", err)
	}
	store := storage.NewStore(enc, db, cfg.Storage.DataDir, logger)
	rm := storage.NewRetentionManager(store, km, cfg.Storage.DataDir, cfg.Storage.RetentionDays, cfg.Storage.SecureDelete, logger)
	return rm.WipeAll(context.Background())
}

func runStatus(cfg *config.Config, logger *slog.Logger) error {
	fmt.Printf("Digital Ghost v%s\n", version)
	fmt.Printf("Data directory : %s\n", cfg.Storage.DataDir)
	fmt.Printf("VLM model      : %s\n", cfg.Inference.Model)
	fmt.Printf("Embed model    : %s\n", cfg.Inference.EmbedModel)
	if cfg.Inference.ChatModel != "" {
		fmt.Printf("Chat model     : %s\n", cfg.Inference.ChatModel)
	}
	fmt.Printf("Capture FPS    : %.1f (idle: %.1f after %ds)\n",
		cfg.Capture.FPS, cfg.Capture.IdleFPS, cfg.Capture.IdleThresholdSec)
	fmt.Printf("Retention      : %d days\n", cfg.Storage.RetentionDays)
	fmt.Printf("Max CPU        : %d%%  Max GPU: %d%%\n", cfg.ResourceBudget.MaxCPUPct, cfg.ResourceBudget.MaxGPUPct)

	// Live store stats — open the store read-only to count nodes.
	km, err := storage.NewKeyManager()
	if err != nil {
		fmt.Printf("Memory nodes   : (keychain unavailable: %v)\n", err)
		return nil
	}
	defer km.Close()
	enc, err := storage.NewEncryptor(km.Key())
	if err != nil {
		fmt.Printf("Memory nodes   : (encryptor unavailable: %v)\n", err)
		return nil
	}
	db, err := storage.NewJSONStore(cfg.Storage.DataDir, enc, logger)
	if err != nil {
		fmt.Printf("Memory nodes   : (store unavailable: %v)\n", err)
		return nil
	}
	store := storage.NewStore(enc, db, cfg.Storage.DataDir, logger)
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, lastCapture, err := store.Stats(ctx)
	if err != nil {
		fmt.Printf("Memory nodes   : (stats error: %v)\n", err)
		return nil
	}
	fmt.Printf("Memory nodes   : %d\n", count)
	if !lastCapture.IsZero() {
		fmt.Printf("Last capture   : %s (%s ago)\n",
			lastCapture.Format("2006-01-02 15:04:05"),
			time.Since(lastCapture).Round(time.Second))
	} else {
		fmt.Printf("Last capture   : none yet\n")
	}
	return nil
}

// ensureDefaultBlocklist writes the embedded default blocklist to path if it
// doesn't already exist. This guarantees the file is present on first run
// regardless of the working directory the binary is launched from.
func ensureDefaultBlocklist(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	if err := os.WriteFile(path, defaults.Blocklist, 0600); err != nil {
		return fmt.Errorf("writing default blocklist: %w", err)
	}
	return nil
}

