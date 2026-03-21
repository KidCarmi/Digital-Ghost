// queue.go implements a bounded frame queue with a drop-oldest policy.
//
// Design rationale:
//   - The queue is a fixed-size ring buffer. No dynamic allocation after init.
//   - When the queue is full, the OLDEST frame is dropped, not the newest.
//     (Old frames are more likely to be stale; the newest frame is more relevant.)
//   - Dropping frames is normal operation, not an error. The capture loop
//     should never block waiting for queue space.
//   - The inference loop reads from the queue when resource budget is available.
package inference

import (
	"log/slog"
	"sync"

	"github.com/KidCarmi/digital-ghost/internal/capture"
)

// Queue is a bounded, thread-safe ring buffer of *capture.Frame.
type Queue struct {
	buf    []*capture.Frame
	head   int // index of the next frame to read
	tail   int // index of the next write position
	count  int
	cap    int
	mu     sync.Mutex
	notEmpty chan struct{}
	logger *slog.Logger
}

// NewQueue creates a Queue with the given capacity.
func NewQueue(capacity int, logger *slog.Logger) *Queue {
	return &Queue{
		buf:      make([]*capture.Frame, capacity),
		cap:      capacity,
		notEmpty: make(chan struct{}, 1),
		logger:   logger,
	}
}

// Push adds a frame to the queue. If the queue is full, the oldest frame is dropped.
// This is a non-blocking operation.
func (q *Queue) Push(frame *capture.Frame) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.count == q.cap {
		// Drop oldest frame (advance head).
		q.head = (q.head + 1) % q.cap
		q.count--
		q.logger.Debug("queue full: dropped oldest frame")
	}

	q.buf[q.tail] = frame
	q.tail = (q.tail + 1) % q.cap
	q.count++

	// Signal that the queue is non-empty (non-blocking signal).
	select {
	case q.notEmpty <- struct{}{}:
	default:
	}
}

// Pop removes and returns the oldest frame from the queue.
// Blocks until a frame is available or the stop channel is closed.
// Returns nil if the stop channel is closed.
func (q *Queue) Pop(stopCh <-chan struct{}) *capture.Frame {
	for {
		q.mu.Lock()
		if q.count > 0 {
			frame := q.buf[q.head]
			q.buf[q.head] = nil // Release reference.
			q.head = (q.head + 1) % q.cap
			q.count--
			q.mu.Unlock()
			return frame
		}
		q.mu.Unlock()

		// Queue is empty; wait for a push signal or stop.
		select {
		case <-stopCh:
			return nil
		case <-q.notEmpty:
			// A frame was pushed; re-check.
		}
	}
}

// Len returns the current number of frames in the queue.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}

// FillRatio returns the queue fullness as a fraction [0.0, 1.0].
func (q *Queue) FillRatio() float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return float64(q.count) / float64(q.cap)
}
