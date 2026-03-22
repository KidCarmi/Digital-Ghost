// Package inference implements resource-safe AI inference for Digital Ghost.
//
// throttle.go implements the resource governor — the component that separates
// "transparent background tool" from "hostile process that makes the laptop unusable."
//
// Design:
//   - Polls CPU and GPU utilization every 2 seconds.
//   - Blocks the inference goroutine when system load exceeds configured thresholds.
//   - Enforces a token-bucket rate limit on VLM calls (max N per minute).
//   - Never blocks the capture goroutine — only inference is throttled.
package inference

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/config"
)

// Governor monitors system resource usage and gates inference calls.
// It is safe for concurrent use.
type Governor struct {
	cfg    config.ResourceBudgetConfig
	logger *slog.Logger

	// cpuPercent and gpuPercent are updated by the metrics goroutine.
	// Read by the inference goroutine. Values are 0–100.
	cpuPercent atomic.Int64
	gpuPercent atomic.Int64

	// prevCPUTotal and prevCPUIdle are the previous /proc/stat cumulative
	// values used to compute a delta-based CPU percentage. Only accessed
	// from the metricsLoop goroutine — no lock needed.
	prevCPUTotal int64
	prevCPUIdle  int64

	// tokenBucket is used to enforce max_inference_per_min.
	tokenBucket chan struct{}

	stopCh chan struct{}
}

// NewGovernor creates and starts a Governor. Call Close() when done.
func NewGovernor(cfg config.ResourceBudgetConfig, logger *slog.Logger) *Governor {
	g := &Governor{
		cfg:    cfg,
		logger: logger,
		stopCh: make(chan struct{}),
	}

	// Start the token bucket with 1 token so the first inference call is
	// not delayed, but without pre-filling the entire capacity — a full
	// pre-fill would allow DG to burst MaxInferencePerMin calls immediately
	// on startup, consuming the sustained-rate budget in seconds.
	g.tokenBucket = make(chan struct{}, cfg.MaxInferencePerMin)
	g.tokenBucket <- struct{}{}

	go g.metricsLoop()
	go g.refillLoop()

	return g
}

// Acquire blocks until:
//   1. System CPU is below cfg.CPUIdleThreshold
//   2. System GPU is below cfg.GPUIdleThreshold
//   3. A token is available in the token bucket
//
// Returns ctx.Err() if the context is cancelled while waiting.
// This is called by the inference goroutine before each VLM call.
func (g *Governor) Acquire(ctx context.Context) error {
	for {
		cpu := g.cpuPercent.Load()
		gpu := g.gpuPercent.Load()

		if cpu > int64(g.cfg.CPUIdleThreshold) {
			g.logger.Debug("governor waiting: CPU above threshold",
				"cpu_pct", cpu, "threshold", g.cfg.CPUIdleThreshold)
			t := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
			continue
		}

		if gpu > int64(g.cfg.GPUIdleThreshold) {
			g.logger.Debug("governor waiting: GPU above threshold",
				"gpu_pct", gpu, "threshold", g.cfg.GPUIdleThreshold)
			t := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
			continue
		}

		// Try to acquire a token from the bucket (non-blocking).
		select {
		case <-g.tokenBucket:
			return nil // Token acquired; proceed with inference.
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Rate limit reached. Block until the next token arrives or ctx
			// is cancelled. No polling loop — the refillLoop will send the
			// token directly into the channel when it's ready, so we just
			// wait on the channel itself. This eliminates the 2s-poll spam
			// in the debug log ("governor waiting: rate limit reached" every
			// 2 seconds) and wakes up instantly on the next refill tick.
			g.logger.Debug("governor waiting: rate limit reached")
			select {
			case <-g.tokenBucket:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// CPUPercent returns the most recently measured CPU utilization (0–100).
func (g *Governor) CPUPercent() int {
	return int(g.cpuPercent.Load())
}

// GPUPercent returns the most recently measured GPU utilization (0–100).
func (g *Governor) GPUPercent() int {
	return int(g.gpuPercent.Load())
}

// DrainBucket empties the token bucket without blocking.
// Call this when Ollama recovers after a crash so the burst of accumulated
// tokens (filled by refillLoop during the outage) doesn't cause an immediate
// volley of inference calls on reconnect.
func (g *Governor) DrainBucket() {
	for {
		select {
		case <-g.tokenBucket:
		default:
			return
		}
	}
}

// Close stops the background goroutines.
func (g *Governor) Close() {
	close(g.stopCh)
}

// metricsLoop polls CPU and GPU every 2 seconds and updates the atomic counters.
func (g *Governor) metricsLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Take an initial reading to avoid a zero value at startup.
	g.updateMetrics()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.updateMetrics()
		}
	}
}

// refillLoop adds tokens to the bucket at a rate of max_inference_per_min.
func (g *Governor) refillLoop() {
	if g.cfg.MaxInferencePerMin <= 0 {
		return
	}
	// Compute refill interval: 1 token every (60s / max_inference_per_min).
	interval := time.Duration(60*float64(time.Second)/float64(g.cfg.MaxInferencePerMin))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			// Non-blocking: if bucket is full, skip.
			select {
			case g.tokenBucket <- struct{}{}:
			default:
			}
		}
	}
}

// updateMetrics reads current CPU and GPU utilization and stores them atomically.
// CPU is computed as a delta between two successive /proc/stat readings so that
// it reflects current load rather than the cumulative average since boot.
func (g *Governor) updateMetrics() {
	total, idle := readCPURaw()
	var cpu int
	if g.prevCPUTotal > 0 {
		dtotal := total - g.prevCPUTotal
		didle := idle - g.prevCPUIdle
		if dtotal > 0 {
			cpu = int(100 * (dtotal - didle) / dtotal)
		}
	}
	g.prevCPUTotal = total
	g.prevCPUIdle = idle

	gpu := readGPUPercent()
	g.cpuPercent.Store(int64(cpu))
	g.gpuPercent.Store(int64(gpu))
}

// readCPURaw and readGPUPercent are implemented in platform-specific files:
//   throttle_unix.go    — Linux/macOS: /proc/stat + sysfs + nvidia-smi
//   throttle_windows.go — Windows: GetSystemTimes + nvidia-smi
