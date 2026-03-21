// Package capture implements screen capture with privacy filtering.
//
// frame.go defines the Frame type and perceptual hash deduplication.
// The perceptual hash (pHash) allows fast detection of near-duplicate frames
// without running them through the expensive VLM inference pipeline.
package capture

import (
	"image"
	"time"

	"github.com/corona10/goimagehash"
)

// Frame is a single captured screen frame together with its window context.
// It is the unit of data flowing from the capture goroutine to the inference queue.
type Frame struct {
	// Image is the captured pixel data. It is valid only until the frame is
	// processed; callers must not retain the image after passing the frame
	// to the queue.
	Image image.Image

	// CapturedAt is the time the frame was captured.
	CapturedAt time.Time

	// WindowCtx contains metadata about the active window at capture time.
	// This is populated before the pixel buffer is read.
	WindowCtx WindowContext

	// PerceptualHash is the pHash computed over the image.
	// Used for deduplication: frames with Hamming distance < threshold are skipped.
	PerceptualHash *goimagehash.ImageHash

	// DisplayIndex identifies which display this frame came from (0-indexed).
	DisplayIndex int
}

// WindowContext mirrors filter.WindowContext but is defined here to avoid
// a circular dependency between capture and filter packages.
// The capture package populates this; the filter package reads it.
type WindowContext struct {
	ProcessName      string
	WindowTitle      string
	BrowserURL       string
	FocusedInputRole string
	PID              int
}

// HashFrame computes the perceptual hash of the frame's image.
// The hash is stored in Frame.PerceptualHash.
// Returns an error if the image is nil or too small to hash.
func HashFrame(f *Frame) error {
	if f.Image == nil {
		return errNilImage
	}
	h, err := goimagehash.PerceptionHash(f.Image)
	if err != nil {
		return err
	}
	f.PerceptualHash = h
	return nil
}

// IsDuplicate returns true if this frame is perceptually similar to prev,
// i.e., the Hamming distance between their pHashes is below threshold.
// A threshold of 10 is appropriate for "essentially identical" frames.
// Returns false (not a duplicate) if either hash is nil.
func IsDuplicate(a, b *Frame, threshold int) (bool, error) {
	if a.PerceptualHash == nil || b.PerceptualHash == nil {
		return false, nil
	}
	dist, err := a.PerceptualHash.Distance(b.PerceptualHash)
	if err != nil {
		return false, err
	}
	return dist < threshold, nil
}

// Capturer is the common interface for all platform screen capture backends.
type Capturer interface {
	Run(stopCh <-chan struct{}) error
	Close() error
}

// frameDuration converts FPS to a time.Duration for the capture ticker.
func frameDuration(fps float64) time.Duration {
	if fps <= 0 {
		fps = 1
	}
	return time.Duration(float64(time.Second) / fps)
}

// errNilImage is returned when a nil image is passed to HashFrame.
var errNilImage = errString("frame image is nil")

type errString string

func (e errString) Error() string { return string(e) }
