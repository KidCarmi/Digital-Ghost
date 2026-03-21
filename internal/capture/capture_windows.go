//go:build windows

// capture_windows.go implements screen capture using the DXGI Desktop Duplication API.
//
// API: IDXGIOutputDuplication (Direct3D 11)
// Permission model: No special permissions required for the user's own desktop session.
// Same API used by: Microsoft Teams, OBS Studio, Xbox Game Bar, Windows Snipping Tool.
//
// Limitations:
//   - Does not work over a basic RDP session with GPU disabled (falls back to GDI).
//   - Requires Direct3D 11 capable GPU (all hardware since 2012).
//   - One IDXGIOutputDuplication interface per monitor; multi-monitor requires
//     enumerating all IDXGIOutput objects on all IDXGIAdapter instances.
//
// Production implementation notes (this file is a scaffold):
//   The production path is:
//     1. D3D11CreateDevice() → *ID3D11Device
//     2. IDXGIDevice → IDXGIAdapter → IDXGIOutput → IDXGIOutput1
//     3. IDXGIOutput1.DuplicateOutput() → *IDXGIOutputDuplication
//     4. IDXGIOutputDuplication.AcquireNextFrame() → DXGI_OUTDUPL_FRAME_INFO + IDXGIResource
//     5. Map the resource as a CPU-readable texture, copy pixels.
//     6. IDXGIOutputDuplication.ReleaseFrame()
//   Use golang.org/x/sys/windows or github.com/go-gl/gl for the D3D11 COM bindings.
package capture

import (
	"fmt"
	"image"
	"log/slog"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KidCarmi/digital-ghost/internal/config"
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
)

const processQueryLimitedInformation = 0x1000

// WindowsCapturer captures frames using DXGI Desktop Duplication.
type WindowsCapturer struct {
	cfg    *config.Config
	gate   *Gate
	logger *slog.Logger
	frames chan<- *Frame
}

// NewCapturer returns the platform capturer for this OS.
func NewCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (Capturer, error) {
	return NewWindowsCapturer(cfg, gate, frames, logger)
}

// NewWindowsCapturer creates a capturer for the primary Windows display.
func NewWindowsCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (*WindowsCapturer, error) {
	// Production: initialize D3D11 device, enumerate DXGI outputs, call DuplicateOutput.
	return &WindowsCapturer{cfg: cfg, gate: gate, frames: frames, logger: logger}, nil
}

// Run starts the DXGI capture loop. Blocks until stopCh is closed.
func (c *WindowsCapturer) Run(stopCh <-chan struct{}) error {
	c.logger.Info("Windows DXGI capture loop started")
	ticker := time.NewTicker(frameDuration(c.cfg.Capture.FPS))
	defer ticker.Stop()

	var prevFrame *Frame

	for {
		select {
		case <-stopCh:
			c.logger.Info("Windows DXGI capture loop stopped")
			return nil
		case <-ticker.C:
			frame, err := c.captureFrame()
			if err != nil {
				c.logger.Warn("DXGI frame capture failed", "error", err)
				continue
			}
			if frame == nil {
				continue // Blocked by privacy gate.
			}

			if prevFrame != nil {
				dup, err := IsDuplicate(frame, prevFrame, c.cfg.Capture.HashThreshold)
				if err == nil && dup {
					continue
				}
			}

			select {
			case c.frames <- frame:
				prevFrame = frame
			default:
				c.logger.Debug("frame queue full; dropping frame")
			}
		}
	}
}

func (c *WindowsCapturer) captureFrame() (*Frame, error) {
	// GATE CHECK MUST BE FIRST — before any pixel buffer allocation.
	result := c.gate.Check()
	if result.Blocked {
		c.logger.Debug("BLOCKED", "reason", result.Reason)
		return nil, nil
	}

	// Production: AcquireNextFrame() → map texture → copy to image.RGBA.
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080)) // stub
	if img == nil {
		return nil, fmt.Errorf("DXGI AcquireNextFrame returned nil")
	}

	frame := &Frame{
		Image:        img,
		CapturedAt:   time.Now(),
		WindowCtx:    WindowContext(result.WindowCtx),
		DisplayIndex: 0,
	}
	if err := HashFrame(frame); err != nil {
		c.logger.Debug("pHash failed", "error", err)
	}
	return frame, nil
}

// Close releases DXGI resources.
// Production: IDXGIOutputDuplication.Release(), ID3D11Device.Release().
func (c *WindowsCapturer) Close() error { return nil }

// queryWindowContextImpl is the Windows implementation of queryWindowContext.
// Uses Win32 APIs to get the foreground window title and process name.
func queryWindowContextImpl() (windowMetadata, error) {
	// 1. GetForegroundWindow() → HWND
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return windowMetadata{ProcessName: "unknown", WindowTitle: ""}, nil
	}

	// 2. GetWindowText → window title
	var titleBuf [512]uint16
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
	title := syscall.UTF16ToString(titleBuf[:])

	// 3. GetWindowThreadProcessId → PID
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

	// 4. OpenProcess + QueryFullProcessImageName → exe path
	processName := "unknown"
	if pid != 0 {
		hProc, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
		if hProc != 0 {
			var exeBuf [windows.MAX_PATH]uint16
			size := uint32(len(exeBuf))
			ret, _, _ := procQueryFullProcessImageNameW.Call(
				hProc,
				0,
				uintptr(unsafe.Pointer(&exeBuf[0])),
				uintptr(unsafe.Pointer(&size)),
			)
			procCloseHandle.Call(hProc)
			if ret != 0 {
				exePath := syscall.UTF16ToString(exeBuf[:size])
				processName = filepath.Base(exePath)
				// Strip .exe suffix for cleaner display.
				if len(processName) > 4 && processName[len(processName)-4:] == ".exe" {
					processName = processName[:len(processName)-4]
				}
			}
		}
	}

	return windowMetadata{
		ProcessName:      processName,
		WindowTitle:      title,
		BrowserURL:       "",
		FocusedInputRole: "",
		PID:              int(pid),
	}, nil
}
