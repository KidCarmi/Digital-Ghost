//go:build linux && x11

// capture_linux_x11.go implements screen capture using X11 XShmGetImage.
//
// API: X11 XShm (MIT-SHM extension)
// Permission model: No special permissions required for the user's own X session.
// Same API used by: OBS Studio, scrot, xwd, gnome-screenshot, ffmpeg.
//
// This implementation does NOT use XRecord, XDamage, or any API that
// requires special X server permissions or could access other users' sessions.
package capture

import (
	"fmt"
	"image"
	"log/slog"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/config"
)

// X11Capturer captures frames from an X11 display using XShmGetImage.
// It implements the Capturer interface.
type X11Capturer struct {
	cfg    *config.Config
	gate   *Gate
	logger *slog.Logger
	frames chan<- *Frame

	// display, root, etc. would be initialized here in production.
	// Represented as uintptr to avoid cgo imports in the architecture scaffold.
	displayPtr uintptr
	rootWindow uintptr
	shmInfo    uintptr
}

// NewCapturer returns the platform capturer for this OS.
func NewCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (Capturer, error) {
	return NewX11Capturer(cfg, gate, frames, logger)
}

// NewX11Capturer creates a capturer for the primary X11 display.
// Returns an error if the display cannot be opened or XShm is unavailable.
func NewX11Capturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (*X11Capturer, error) {
	// The X11 window metadata implementation is not yet complete.
	// Without real process name / window title data, the privacy gate cannot
	// function correctly (it would always see "stub" and never block anything).
	// Refusing to start is the correct fail-closed behaviour.
	return nil, fmt.Errorf(
		"X11 capture is not yet implemented: " +
			"queryWindowContextImpl() returns stub metadata, which means the " +
			"privacy gate cannot block sensitive windows. " +
			"Build and run Digital Ghost on Windows until the X11 implementation is complete.",
	)
}

// Run starts the capture loop. It blocks until ctx is cancelled.
// Run should be called in a dedicated goroutine.
func (c *X11Capturer) Run(stopCh <-chan struct{}) error {
	c.logger.Info("X11 capture loop started")
	ticker := time.NewTicker(frameDuration(c.cfg.Capture.FPS))
	defer ticker.Stop()

	var prevFrame *Frame

	for {
		select {
		case <-stopCh:
			c.logger.Info("X11 capture loop stopped")
			return nil
		case <-ticker.C:
			frame, err := c.captureFrame()
			if err != nil {
				c.logger.Warn("frame capture failed", "error", err)
				continue
			}
			if frame == nil {
				// Blocked by privacy gate — already logged in gate.Check().
				continue
			}

			// Perceptual hash deduplication.
			if prevFrame != nil {
				dup, err := IsDuplicate(frame, prevFrame, c.cfg.Capture.HashThreshold)
				if err != nil {
					c.logger.Debug("pHash comparison failed", "error", err)
				} else if dup {
					c.logger.Debug("duplicate frame skipped", "hamming_dist", "<threshold")
					continue
				}
			}

			// Non-blocking send: if queue is full, drop oldest (handled by queue.go).
			// We drop HERE by selecting on default rather than blocking the capture loop.
			select {
			case c.frames <- frame:
				prevFrame = frame
			default:
				c.logger.Debug("frame queue full; dropping frame")
			}
		}
	}
}

// captureFrame runs the privacy gate and, if allowed, reads a frame from the display.
// Returns nil (with no error) if the gate blocks the frame.
func (c *X11Capturer) captureFrame() (*Frame, error) {
	// GATE CHECK MUST BE FIRST — before any pixel buffer allocation.
	result := c.gate.Check()
	if result.Blocked {
		c.logger.Debug("BLOCKED", "reason", result.Reason)
		return nil, nil
	}

	c.logger.Debug("PASS", "process", result.WindowCtx.ProcessName, "title", result.WindowCtx.WindowTitle)

	// In production: XShmGetImage(display, root, ximage, 0, 0, AllPlanes)
	// then convert ximage to image.RGBA.
	// Stub: return a placeholder image for architecture scaffold.
	img := captureDisplayStub()
	if img == nil {
		return nil, fmt.Errorf("XShmGetImage returned nil")
	}

	frame := &Frame{
		Image:        img,
		CapturedAt:   time.Now(),
		WindowCtx:    WindowContext(result.WindowCtx),
		DisplayIndex: 0,
	}

	if err := HashFrame(frame); err != nil {
		// pHash failure is non-fatal; frame proceeds without hash (dedup skipped).
		c.logger.Debug("pHash computation failed", "error", err)
	}

	return frame, nil
}

// Close releases X11 resources (XShmDetach, shmdt, XFreeGC, XCloseDisplay).
func (c *X11Capturer) Close() error {
	// In production: XShmDetach + shmdt + XCloseDisplay.
	return nil
}

// queryWindowContextImpl is the X11 implementation of queryWindowContext.
// It reads _NET_ACTIVE_WINDOW from the root window, then queries the
// AT-SPI2 accessibility tree for browser URL and focused input role.
//
// NOT YET IMPLEMENTED: returns an error so the privacy gate fails closed.
// The gate's Check() treats any error here as a block decision, which is
// the correct safe behaviour until the real implementation ships.
func queryWindowContextImpl() (windowMetadata, error) {
	return windowMetadata{}, fmt.Errorf(
		"X11 window metadata not yet implemented: " +
			"privacy gate cannot function without real process/window information",
	)
}

// captureDisplayStub returns a placeholder image for the architecture scaffold.
// Production: remove this and use actual XShmGetImage output.
func captureDisplayStub() image.Image {
	return image.NewRGBA(image.Rect(0, 0, 1920, 1080))
}

