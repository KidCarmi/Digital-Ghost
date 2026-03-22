// privacy.go is the pre-capture privacy gate.
//
// ★ THIS IS THE HIGHEST-CONSEQUENCE FILE IN THE CODEBASE. ★
//
// Every other safety property in Digital Ghost depends on this gate being correct.
// It must be the first file reviewed and the last file changed.
//
// Contract:
//   - Window metadata (process name, title, URL) is queried BEFORE any pixel is read.
//   - If the gate returns Block, no pixel buffer is allocated for this frame.
//   - The gate is fail-closed: if metadata cannot be obtained, the frame is blocked.
//   - There is no "bypass" flag, "emergency override", or debug mode that skips the gate.
//
// Review checklist (must be verified on every change to this file):
//   [ ] Check() calls the blocklist BEFORE allocating any pixel buffer
//   [ ] Check() returns Blocked=true on ALL error paths
//   [ ] No code path in Check() allocates pixel data if Blocked=true
//   [ ] The gate cannot be disabled by any runtime flag or environment variable
//   [ ] All platform implementations of queryWindowContext() fail-close on error
package capture

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/KidCarmi/digital-ghost/internal/filter"
)

// Gate is the pre-capture privacy gate.
// Create one per process; it is safe for concurrent use.
type Gate struct {
	blocklist *filter.Blocklist
	logger    *slog.Logger
	paused    atomic.Bool // set by tray pause/resume; never persisted to disk
}

// Pause suspends capture. The gate still evaluates window metadata on every
// tick so that blocked windows (password managers, banking sites) are detected
// even while paused, and captures are correctly suppressed on resume.
func (g *Gate) Pause() { g.paused.Store(true) }

// Resume re-enables capture. The next Check() call evaluates the gate fresh —
// if the foreground window changed to a blocked app while paused, it will
// still be blocked on resume.
func (g *Gate) Resume() { g.paused.Store(false) }

// GateResult describes the gate's decision for a single capture opportunity.
type GateResult struct {
	Blocked bool
	Reason  string
	// WindowCtx is populated only when Blocked=false, for use by downstream components.
	WindowCtx filter.WindowContext
}

// NewGate creates a Gate backed by the given blocklist.
func NewGate(bl *filter.Blocklist, logger *slog.Logger) *Gate {
	return &Gate{blocklist: bl, logger: logger}
}

// Check queries the active window's metadata and decides whether to allow capture.
// It must be called BEFORE allocating any pixel buffer.
//
// The caller is responsible for ensuring no pixel buffer is allocated if
// Check returns GateResult{Blocked: true}.
func (g *Gate) Check() GateResult {
	// Step 1: Query window metadata. This is zero-cost (no pixel read).
	ctx, err := queryWindowContext()
	if err != nil {
		// Cannot determine window context — fail closed.
		g.logger.Debug("window context query failed; blocking frame", "error", err)
		return GateResult{
			Blocked: true,
			Reason:  fmt.Sprintf("metadata_query_failed:%v", err),
		}
	}

	// Step 2: Apply the blocklist to the foreground window.
	decision := g.blocklist.Check(filter.WindowContext{
		ProcessName:      ctx.ProcessName,
		WindowTitle:      ctx.WindowTitle,
		BrowserURL:       ctx.BrowserURL,
		FocusedInputRole: ctx.FocusedInputRole,
		PID:              ctx.PID,
	})

	if decision.Blocked {
		g.logger.Debug("frame blocked by privacy gate",
			"process", ctx.ProcessName,
			"reason", decision.Reason)
		return GateResult{Blocked: true, Reason: decision.Reason}
	}

	// Step 2b: ARCH-5 — check all OTHER visible windows (not just foreground).
	// A password manager open on display 2 behind another window must block capture
	// even though it is not the foreground window. We only need the process name
	// for each background window; titles and URLs are not checked for performance.
	for _, bgProc := range queryVisibleBackgroundProcesses() {
		bgDecision := g.blocklist.Check(filter.WindowContext{ProcessName: bgProc})
		if bgDecision.Blocked {
			g.logger.Debug("frame blocked: background window matches blocklist",
				"process", bgProc, "reason", bgDecision.Reason)
			return GateResult{
				Blocked: true,
				Reason:  fmt.Sprintf("background_window:%s", bgDecision.Reason),
			}
		}
	}

	// Step 3: Check for sensitive input role even if process is not on the blocklist.
	// This catches password fields in otherwise-allowed applications (e.g., a browser
	// not currently on a banking URL, but with a focused password input).
	if ctx.FocusedInputRole != "" {
		sensitiveRoles := map[string]bool{
			"password":   true,
			"spinbutton": true, // Common for OTP fields.
		}
		if sensitiveRoles[ctx.FocusedInputRole] {
			g.logger.Debug("frame blocked: sensitive input role focused",
				"role", ctx.FocusedInputRole,
				"process", ctx.ProcessName)
			return GateResult{
				Blocked: true,
				Reason:  fmt.Sprintf("sensitive_input_focused:%s", ctx.FocusedInputRole),
			}
		}
	}

	// Step 4: Check if the user has paused capture via the tray icon.
	// The gate runs all checks above even while paused so that blocked windows
	// (password managers, banking URLs) are detected during the pause period.
	// On resume, the next Check() sees the current window state fresh.
	if g.paused.Load() {
		return GateResult{Blocked: true, Reason: "capture_paused"}
	}

	return GateResult{
		Blocked: false,
		Reason:  "pass",
		WindowCtx: filter.WindowContext{
			ProcessName:      ctx.ProcessName,
			WindowTitle:      ctx.WindowTitle,
			BrowserURL:       ctx.BrowserURL,
			FocusedInputRole: ctx.FocusedInputRole,
			PID:              ctx.PID,
		},
	}
}

// windowMetadata is the raw OS-level window metadata before conversion
// to filter.WindowContext.
type windowMetadata struct {
	ProcessName      string
	WindowTitle      string
	BrowserURL       string
	FocusedInputRole string
	PID              int
}

// queryWindowContext queries the OS for the currently-active window's metadata.
// This function is implemented per-platform:
//   - capture_linux_x11.go: X11 _NET_ACTIVE_WINDOW + AT-SPI2 accessibility tree
//   - capture_linux_wayland.go: xdg-desktop-portal + AT-SPI2
//   - capture_darwin.go: NSWorkspace + AXUIElement
//   - capture_windows.go: GetForegroundWindow + UI Automation
//
// Contract: returns an error if metadata cannot be obtained. The caller
// (Check) will treat any error as a block decision.
func queryWindowContext() (windowMetadata, error) {
	return queryWindowContextImpl()
}

// queryVisibleBackgroundProcesses returns the process names of all visible
// top-level windows that are NOT the current foreground window.
// Used by Check() to block capture when a sensitive app is visible on any monitor.
// Returns nil on platforms that do not implement this (non-Windows).
// Implemented per platform:
//   - capture_windows.go: EnumWindows + IsWindowVisible + process name lookup
//   - capture_unsupported.go + capture_linux_x11.go: returns nil (graceful no-op)
func queryVisibleBackgroundProcesses() []string {
	return queryVisibleBackgroundProcessesImpl()
}
