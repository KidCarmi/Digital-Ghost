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
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KidCarmi/digital-ghost/internal/config"
)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	gdi32                          = syscall.NewLazyDLL("gdi32.dll")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")
	procGetWindowThreadPID         = user32.NewProc("GetWindowThreadProcessId")
	procGetSystemMetrics           = user32.NewProc("GetSystemMetrics")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procCreateDC                   = gdi32.NewProc("CreateDCW")
	procDeleteDC                   = gdi32.NewProc("DeleteDC")
	procCreateCompatibleDC         = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap     = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject               = gdi32.NewProc("SelectObject")
	procDeleteObject               = gdi32.NewProc("DeleteObject")
	procBitBlt                     = gdi32.NewProc("BitBlt")
	procGetDIBits                  = gdi32.NewProc("GetDIBits")
	procGetLastInputInfo            = user32.NewProc("GetLastInputInfo")
	procGetTickCount                = kernel32.NewProc("GetTickCount")
	procEnumDisplayMonitors         = user32.NewProc("EnumDisplayMonitors")
	procEnumChildWindows            = user32.NewProc("EnumChildWindows")
	procGetClassNameW               = user32.NewProc("GetClassNameW")
	procSendMessageW                = user32.NewProc("SendMessageW")
)

const (
	processQueryLimitedInformation = 0x1000
	srccopy                        = 0x00CC0020
	dibRGBColors                   = 0
	smCxScreen                     = 0
	smCyScreen                     = 1
	wmGetText                      = 0x000D
)

// bitmapInfoHeader mirrors the Win32 BITMAPINFOHEADER structure.
type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// lastInputInfo mirrors the Win32 LASTINPUTINFO structure.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// monitorInfo holds a monitor handle and its bounding rectangle.
type monitorInfo struct {
	hMonitor uintptr
	rect     image.Rectangle
}

// browserURLState is the shared state threaded through the EnumChildWindows
// callback via lParam. Declared at package level so the callback function
// (also package-level) can reference it as a typed pointer.
type browserURLState struct {
	url   string
	found bool
}

// enumMonitorsCallback and enumChildWindowsCallback are created once at
// package init. syscall.NewCallback has a hard limit of 1024 total
// registrations — calling it per-frame exhausts the pool in minutes.
var (
	enumMonitorsCallback    = syscall.NewCallback(enumMonitorProc)
	enumChildWindowsCallback = syscall.NewCallback(enumChildWindowProc)
)

// enumMonitorProc is the EnumDisplayMonitors callback.
// State (a *[]monitorInfo) is passed via lParam to avoid a closure.
func enumMonitorProc(hMon, _ /*hdcMon*/, lprcMon, lParam uintptr) uintptr {
	type rect32 struct{ left, top, right, bottom int32 }
	r := (*rect32)(unsafe.Pointer(lprcMon))
	monitors := (*[]monitorInfo)(unsafe.Pointer(lParam))
	*monitors = append(*monitors, monitorInfo{
		hMonitor: hMon,
		rect:     image.Rect(int(r.left), int(r.top), int(r.right), int(r.bottom)),
	})
	return 1 // continue enumeration
}

// enumChildWindowProc is the EnumChildWindows callback used by extractBrowserURL.
// State (a *browserURLState) is passed via lParam to avoid a closure.
func enumChildWindowProc(childHwnd, lParam uintptr) uintptr {
	state := (*browserURLState)(unsafe.Pointer(lParam))
	if state.found {
		return 0 // stop enumeration
	}
	var classBuf [256]uint16
	procGetClassNameW.Call(childHwnd, uintptr(unsafe.Pointer(&classBuf[0])), uintptr(len(classBuf)))
	className := syscall.UTF16ToString(classBuf[:])
	if className == "Chrome_OmniboxView" || className == "OmniboxViewViews" {
		var textBuf [2048]uint16
		procSendMessageW.Call(childHwnd, wmGetText, uintptr(len(textBuf)), uintptr(unsafe.Pointer(&textBuf[0])))
		text := syscall.UTF16ToString(textBuf[:])
		if text != "" {
			state.url = text
			state.found = true
		}
		return 0 // stop enumeration
	}
	return 1 // continue
}

// WindowsCapturer captures frames using DXGI Desktop Duplication.
type WindowsCapturer struct {
	cfg           *config.Config
	gate          *Gate
	logger        *slog.Logger
	frames        chan<- *Frame
	lastWindowKey string
	windowSince   time.Time
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
	c.logger.Info("Windows GDI capture loop started")
	ticker := time.NewTicker(frameDuration(c.cfg.Capture.FPS))
	defer ticker.Stop()

	var prevFrame *Frame

	for {
		select {
		case <-stopCh:
			c.logger.Info("Windows GDI capture loop stopped")
			return nil
		case <-ticker.C:
			frame, err := c.captureFrame()
			if err != nil {
				c.logger.Warn("GDI frame capture failed", "error", err)
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

// secondsSinceLastInput returns how many seconds have elapsed since the last
// keyboard or mouse input event, using GetLastInputInfo (polling, not hooking).
// Returns 0 on error (conservative — treats as recent input).
func secondsSinceLastInput() float64 {
	info := lastInputInfo{cbSize: 8} // sizeof(LASTINPUTINFO)
	ret, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0
	}
	tick, _, _ := procGetTickCount.Call()
	elapsed := uint32(tick) - info.dwTime
	return float64(elapsed) / 1000.0
}

// enumerateMonitors returns the list of monitors and their virtual-screen rectangles.
// Uses EnumDisplayMonitors with a null HDC to cover the full virtual screen.
// The callback is package-level (enumMonitorsCallback) — created once at init.
func enumerateMonitors() []monitorInfo {
	var monitors []monitorInfo
	procEnumDisplayMonitors.Call(0, 0, enumMonitorsCallback, uintptr(unsafe.Pointer(&monitors)))
	return monitors
}

// captureMonitorGDI captures a single monitor's pixels using GDI BitBlt.
// rect is the monitor's position in virtual-screen coordinates.
// Returns an *image.RGBA in top-down order with RGBA byte layout.
func captureMonitorGDI(rect image.Rectangle) (*image.RGBA, error) {
	width := rect.Dx()
	height := rect.Dy()
	if width == 0 || height == 0 {
		return nil, fmt.Errorf("monitor has zero dimensions")
	}

	display, err := syscall.UTF16PtrFromString("DISPLAY")
	if err != nil {
		return nil, fmt.Errorf("UTF16PtrFromString: %w", err)
	}

	hScreenDC, _, _ := procCreateDC.Call(uintptr(unsafe.Pointer(display)), 0, 0, 0)
	if hScreenDC == 0 {
		return nil, fmt.Errorf("CreateDC(DISPLAY) failed")
	}
	defer procDeleteDC.Call(hScreenDC)

	hMemDC, _, _ := procCreateCompatibleDC.Call(hScreenDC)
	if hMemDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(hMemDC)

	hBitmap, _, _ := procCreateCompatibleBitmap.Call(hScreenDC, uintptr(width), uintptr(height))
	if hBitmap == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(hBitmap)

	procSelectObject.Call(hMemDC, hBitmap)

	// BitBlt from the monitor's virtual-screen origin.
	ret, _, _ := procBitBlt.Call(
		hMemDC, 0, 0, uintptr(width), uintptr(height),
		hScreenDC, uintptr(rect.Min.X), uintptr(rect.Min.Y),
		srccopy,
	)
	if ret == 0 {
		return nil, fmt.Errorf("BitBlt failed")
	}

	// GetDIBits extracts raw pixel bytes. biHeight < 0 = top-down DIB.
	bmi := bitmapInfoHeader{
		biSize:     40, // sizeof(BITMAPINFOHEADER)
		biWidth:    int32(width),
		biHeight:   -int32(height),
		biPlanes:   1,
		biBitCount: 32,
		// biCompression = 0 = BI_RGB
	}
	pixels := make([]byte, width*height*4)
	ret, _, _ = procGetDIBits.Call(
		hScreenDC,
		hBitmap,
		0,
		uintptr(height),
		uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
	)
	if ret == 0 {
		return nil, fmt.Errorf("GetDIBits failed")
	}

	// GDI returns pixels in BGRA order; image.RGBA expects RGBA.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Pix[i*4+0] = pixels[i*4+2] // R ← B
		img.Pix[i*4+1] = pixels[i*4+1] // G ← G
		img.Pix[i*4+2] = pixels[i*4+0] // B ← R
		img.Pix[i*4+3] = 0xFF
	}
	return img, nil
}

// extractBrowserURL attempts to read the current URL from a browser's address bar
// using EnumChildWindows to find the Chromium omnibox edit control, then
// WM_GETTEXT to read its text. Falls back to "" on any error (fail-closed).
//
// Supported engines:
//   - Chromium-based (Chrome, Edge, Opera, Brave, Vivaldi, Chromium): "Chrome_OmniboxView"
//   - Firefox: "MozillaWindowClass" toolbar — reads child edit control
//
// The EnumChildWindows callback is package-level (enumChildWindowsCallback) —
// created once at init to avoid exhausting syscall.NewCallback's 1024-slot pool.
func extractBrowserURL(hwnd uintptr, processName string) string {
	knownBrowsers := []string{"opera", "chrome", "firefox", "msedge", "brave", "vivaldi", "chromium"}
	lowerProcess := strings.ToLower(processName)
	isBrowser := false
	for _, b := range knownBrowsers {
		if strings.Contains(lowerProcess, b) {
			isBrowser = true
			break
		}
	}
	if !isBrowser {
		return ""
	}

	state := &browserURLState{}
	procEnumChildWindows.Call(hwnd, enumChildWindowsCallback, uintptr(unsafe.Pointer(state)))
	return state.url
}

func (c *WindowsCapturer) captureFrame() (*Frame, error) {
	// GATE CHECK MUST BE FIRST — before any pixel buffer allocation.
	result := c.gate.Check()
	if result.Blocked {
		c.logger.Debug("BLOCKED", "reason", result.Reason)
		return nil, nil
	}

	// Compute dwell time for the current foreground window.
	// Reset on any window change — do not inherit dwell from previously blocked windows.
	windowKey := result.WindowCtx.ProcessName + "|" + result.WindowCtx.WindowTitle
	now := time.Now()
	if windowKey != c.lastWindowKey {
		c.lastWindowKey = windowKey
		c.windowSince = now
	}
	dwellSeconds := now.Sub(c.windowSince).Seconds()

	sinceInput := secondsSinceLastInput()

	// Enumerate monitors and capture each one.
	monitors := enumerateMonitors()
	if len(monitors) == 0 {
		// Fallback: capture primary monitor using system metrics.
		w, _, _ := procGetSystemMetrics.Call(smCxScreen)
		h, _, _ := procGetSystemMetrics.Call(smCyScreen)
		monitors = []monitorInfo{{rect: image.Rect(0, 0, int(w), int(h))}}
	}

	// Capture the first monitor and return a single frame.
	// Multi-monitor: one frame per display pushed to c.frames in Run().
	// For now captureFrame returns the primary-monitor frame; callers that want
	// all monitors should call captureAllMonitors() directly.
	img, err := captureMonitorGDI(monitors[0].rect)
	if err != nil {
		return nil, fmt.Errorf("GDI capture: %w", err)
	}

	frame := &Frame{
		Image:             img,
		CapturedAt:        now,
		WindowCtx:         WindowContext(result.WindowCtx),
		DisplayIndex:      0,
		DwellSeconds:      dwellSeconds,
		SecondsSinceInput: sinceInput,
	}
	if err := HashFrame(frame); err != nil {
		c.logger.Debug("pHash failed", "error", err)
	}
	return frame, nil
}

// captureAllMonitors captures every connected monitor and pushes one Frame per
// display to c.frames. This is called by Run() to enable multi-monitor support.
func (c *WindowsCapturer) captureAllMonitors(result GateResult, dwellSeconds, sinceInput float64, now time.Time) {
	monitors := enumerateMonitors()
	if len(monitors) == 0 {
		w, _, _ := procGetSystemMetrics.Call(smCxScreen)
		h, _, _ := procGetSystemMetrics.Call(smCyScreen)
		monitors = []monitorInfo{{rect: image.Rect(0, 0, int(w), int(h))}}
	}

	for i, mon := range monitors {
		img, err := captureMonitorGDI(mon.rect)
		if err != nil {
			c.logger.Warn("GDI monitor capture failed", "display_index", i, "error", err)
			continue
		}
		frame := &Frame{
			Image:             img,
			CapturedAt:        now,
			WindowCtx:         WindowContext(result.WindowCtx),
			DisplayIndex:      i,
			DwellSeconds:      dwellSeconds,
			SecondsSinceInput: sinceInput,
		}
		if err := HashFrame(frame); err != nil {
			c.logger.Debug("pHash failed", "display_index", i, "error", err)
		}
		select {
		case c.frames <- frame:
		default:
			c.logger.Debug("frame queue full; dropping frame", "display_index", i)
		}
	}
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

	browserURL := extractBrowserURL(hwnd, processName)

	return windowMetadata{
		ProcessName:      processName,
		WindowTitle:      title,
		BrowserURL:       browserURL,
		FocusedInputRole: "",
		PID:              int(pid),
	}, nil
}
