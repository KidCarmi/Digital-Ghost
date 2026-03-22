// Package capture implements screen capture with privacy filtering.
//
// frame.go defines the Frame type and perceptual hash deduplication.
// The perceptual hash (pHash) allows fast detection of near-duplicate frames
// without running them through the expensive VLM inference pipeline.
package capture

import (
	"image"
	"image/color"
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

	// DwellSeconds is how long the foreground window was active before this frame was captured.
	DwellSeconds float64

	// SecondsSinceInput is seconds since the last keyboard/mouse event (from GetLastInputInfo).
	SecondsSinceInput float64
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

	// ActiveWindowRect is the bounding rectangle of the foreground window in
	// image-local coordinates (already translated from virtual-screen coords).
	// Used by the inference layer to crop the frame before VLM encoding.
	// Zero value means the rect is unavailable; inference falls back to full frame.
	ActiveWindowRect image.Rectangle
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

// queueTargetW and queueTargetH are the dimensions frames are downscaled to
// before being placed in the inference queue.  Keeping frames small reduces
// peak queue memory from ~830 MB (100 × 1920×1080 RGBA) to ~100 MB.
// The VLM (llava:7b CLIP) operates at 336×336 internally, so 672×378 already
// gives it 2× more information than the original full-HD source.
const queueTargetW, queueTargetH = 672, 378

// DownscaleForQueue resizes frame.Image to fit within queueTargetW×queueTargetH
// (maintaining aspect ratio via uniform scale) using nearest-neighbour sampling,
// and scales frame.WindowCtx.ActiveWindowRect proportionally.
//
// pHash must be computed BEFORE calling this function, since the hash is derived
// from the full-resolution image for accurate deduplication.
//
// Frames that are already at or below the target dimensions are left unchanged.
func DownscaleForQueue(f *Frame) {
	if f.Image == nil {
		return
	}
	bounds := f.Image.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= queueTargetW && srcH <= queueTargetH {
		return // already small enough
	}

	// Uniform scale: pick the factor that brings both dimensions within bounds.
	scaleX := float64(queueTargetW) / float64(srcW)
	scaleY := float64(queueTargetH) / float64(srcH)
	scale := scaleX
	if scaleY < scale {
		scale = scaleY
	}
	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	f.Image = nearestNeighbor(f.Image, dstW, dstH)

	// Scale the active window rect by the same factor so that the inference
	// layer can still crop to it correctly.
	r := f.WindowCtx.ActiveWindowRect
	if r.Dx() > 0 && r.Dy() > 0 {
		f.WindowCtx.ActiveWindowRect = image.Rect(
			int(float64(r.Min.X)*scale),
			int(float64(r.Min.Y)*scale),
			int(float64(r.Max.X)*scale),
			int(float64(r.Max.Y)*scale),
		)
	}
}

// nearestNeighbor resizes src to exactly w×h using nearest-neighbour sampling.
func nearestNeighbor(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	scaleX := float64(sb.Dx()) / float64(w)
	scaleY := float64(sb.Dy()) / float64(h)
	for y := 0; y < h; y++ {
		srcY := sb.Min.Y + int(float64(y)*scaleY)
		for x := 0; x < w; x++ {
			srcX := sb.Min.X + int(float64(x)*scaleX)
			r, g, b, a := src.At(srcX, srcY).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(b >> 8),
				A: uint8(a >> 8),
			})
		}
	}
	return dst
}

// errNilImage is returned when a nil image is passed to HashFrame.
var errNilImage = errString("frame image is nil")

type errString string

func (e errString) Error() string { return string(e) }
