//go:build !windows && !(linux && x11)

// capture_unsupported.go provides stub implementations for platforms where
// no capture backend has been implemented yet (e.g. plain Linux without X11,
// macOS, or CI environments building for type-checking only).
package capture

import (
	"errors"
	"image"
	"log/slog"

	"github.com/KidCarmi/digital-ghost/internal/config"
)

var errUnsupportedPlatform = errors.New("screen capture is not supported on this platform")

// UnsupportedCapturer is a no-op capturer used on unsupported platforms.
type UnsupportedCapturer struct{}

// NewCapturer returns the platform capturer for this OS.
func NewCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (Capturer, error) {
	return NewUnsupportedCapturer(cfg, gate, frames, logger)
}

func NewUnsupportedCapturer(_ *config.Config, _ *Gate, _ chan<- *Frame, _ *slog.Logger) (*UnsupportedCapturer, error) {
	return &UnsupportedCapturer{}, nil
}

func (c *UnsupportedCapturer) Run(_ <-chan struct{}) error {
	return errUnsupportedPlatform
}

func (c *UnsupportedCapturer) Close() error { return nil }

func queryWindowContextImpl() (windowMetadata, error) {
	return windowMetadata{}, errUnsupportedPlatform
}

// queryVisibleBackgroundProcessesImpl returns nil on unsupported platforms.
// Background window enumeration is only implemented on Windows.
func queryVisibleBackgroundProcessesImpl() []string { return nil }

func captureDisplayStub() image.Image {
	return image.NewRGBA(image.Rect(0, 0, 1920, 1080))
}
