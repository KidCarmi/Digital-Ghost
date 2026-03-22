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
	"runtime"
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
	procEnumWindows                 = user32.NewProc("EnumWindows")
	procIsWindowVisible             = user32.NewProc("IsWindowVisible")
	procGetClassNameW               = user32.NewProc("GetClassNameW")
	procSendMessageW                = user32.NewProc("SendMessageW")
	procGetWindowRect               = user32.NewProc("GetWindowRect")
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

// winRECT mirrors the Win32 RECT structure used by GetWindowRect.
type winRECT struct{ Left, Top, Right, Bottom int32 }

// queryActiveWindowRect returns the bounding rectangle of the foreground window
// in virtual-screen coordinates. Returns an empty rectangle on any error.
func queryActiveWindowRect() image.Rectangle {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return image.Rectangle{}
	}
	var r winRECT
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return image.Rectangle{}
	}
	return image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
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

// WindowsCapturer is the top-level Windows capture coordinator.
// It owns the privacy gate, the output channel, and the DXGI session (if any).
// Per-monitor state (GDI handles, pHash history) lives in monitorLoop instances
// created by Run().
type WindowsCapturer struct {
	cfg    *config.Config
	gate   *Gate
	logger *slog.Logger
	frames chan<- *Frame
	// dxgi is non-nil when cfg.Capture.Backend == "dxgi".
	dxgi *DXGICapturer
}

// NewCapturer returns the platform capturer for this OS.
func NewCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (Capturer, error) {
	return NewWindowsCapturer(cfg, gate, frames, logger)
}

// NewWindowsCapturer creates a capturer for the primary Windows display.
// When cfg.Capture.Backend is "dxgi", a DXGI session is opened immediately.
func NewWindowsCapturer(cfg *config.Config, gate *Gate, frames chan<- *Frame, logger *slog.Logger) (*WindowsCapturer, error) {
	c := &WindowsCapturer{cfg: cfg, gate: gate, frames: frames, logger: logger}
	if cfg.Capture.Backend == "dxgi" {
		dxgi, err := NewDXGICapturer(c, logger)
		if err != nil {
			logger.Warn("DXGI backend unavailable; falling back to GDI", "error", err)
		} else {
			c.dxgi = dxgi
			logger.Info("DXGI capture backend initialised")
		}
	}
	return c, nil
}

// Run starts one capture goroutine per connected monitor. Blocks until stopCh
// is closed and all per-monitor goroutines have exited.
func (c *WindowsCapturer) Run(stopCh <-chan struct{}) error {
	backend := "GDI"
	if c.dxgi != nil {
		backend = "DXGI"
	}

	monitors := enumerateMonitors()
	if len(monitors) == 0 {
		w, _, _ := procGetSystemMetrics.Call(smCxScreen)
		h, _, _ := procGetSystemMetrics.Call(smCyScreen)
		monitors = []monitorInfo{{rect: image.Rect(0, 0, int(w), int(h))}}
	}

	c.logger.Info("Windows capture started",
		"backend", backend,
		"monitors", len(monitors))

	done := make(chan struct{})
	for i, mon := range monitors {
		ml := &monitorLoop{
			parent:     c,
			mon:        mon,
			displayIdx: i,
		}
		go func() {
			defer func() { done <- struct{}{} }()
			ml.run(stopCh)
		}()
	}

	for range monitors {
		<-done
	}
	c.logger.Info("Windows capture stopped", "backend", backend)
	return nil
}

// monitorLoop owns the per-monitor capture state: GDI handles, pHash memory,
// dwell tracking, and display index. One instance is created per connected monitor in Run().
type monitorLoop struct {
	parent     *WindowsCapturer
	mon        monitorInfo
	displayIdx int

	// Per-monitor GDI handle cache.
	screenDC uintptr
	memDC    uintptr
	bitmap   uintptr
	cachedW  int
	cachedH  int

	prevFrame *Frame

	// Per-monitor dwell tracking (each display has independent window history).
	lastWindowKey string
	windowSince   time.Time
}

// run is the per-monitor capture ticker loop. Blocks until stopCh is closed.
// Automatically switches to IdleFPS when no keyboard/mouse input has been
// detected for IdleThresholdSec seconds, saving GPU/CPU during idle periods.
func (ml *monitorLoop) run(stopCh <-chan struct{}) {
	// Lock this goroutine to its OS thread for the entire capture loop.
	// COM (UIA for password detection + browser URL) and DXGI are both
	// apartment-threaded: CoInitializeEx and DXGI COM calls must be made
	// from the same OS thread. Without this, Go's scheduler can migrate
	// the goroutine mid-frame, causing CoUninitialize on the wrong thread
	// (a no-op) and leaving COM state dangling on the original thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	c := ml.parent
	ticker := time.NewTicker(frameDuration(c.cfg.Capture.FPS))
	defer ticker.Stop()
	defer ml.releaseHandles()

	isIdle := false
	idleThreshold := float64(c.cfg.Capture.IdleThresholdSec)
	if idleThreshold <= 0 {
		idleThreshold = 30
	}

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			// Switch ticker rate based on idle state (check once per tick, zero cost).
			idle := secondsSinceLastInput() > idleThreshold
			if idle != isIdle {
				isIdle = idle
				ticker.Stop()
				if isIdle {
					ticker = time.NewTicker(frameDuration(c.cfg.Capture.IdleFPS))
					c.logger.Debug("capture rate reduced: system idle",
						"display", ml.displayIdx,
						"fps", c.cfg.Capture.IdleFPS)
				} else {
					ticker = time.NewTicker(frameDuration(c.cfg.Capture.FPS))
					c.logger.Debug("capture rate restored: input detected",
						"display", ml.displayIdx,
						"fps", c.cfg.Capture.FPS)
				}
			}

			frame, err := ml.captureFrame()
			if err != nil {
				c.logger.Warn("frame capture failed",
					"display", ml.displayIdx, "error", err)
				continue
			}
			if frame == nil {
				continue // gate blocked or DXGI timeout
			}

			if ml.prevFrame != nil {
				dup, err := IsDuplicate(frame, ml.prevFrame, c.cfg.Capture.HashThreshold)
				if err == nil && dup {
					continue
				}
			}

			select {
			case c.frames <- frame:
				ml.prevFrame = frame
			default:
				c.logger.Warn("frame queue full; dropping frame — inference too slow or queue too small",
					"display", ml.displayIdx)
			}
		}
	}
}

// captureFrame runs a single capture tick for this monitor.
// Gate check happens here — if blocked, returns (nil, nil).
func (ml *monitorLoop) captureFrame() (*Frame, error) {
	c := ml.parent

	// Privacy gate is global: checks foreground window across all displays.
	result := c.gate.Check()
	if result.Blocked {
		c.logger.Debug("BLOCKED", "reason", result.Reason, "display", ml.displayIdx)
		return nil, nil
	}

	// Dwell tracking — per monitorLoop, no lock needed (each goroutine owns its own instance).
	windowKey := result.WindowCtx.ProcessName + "|" + result.WindowCtx.WindowTitle
	now := time.Now()
	if windowKey != ml.lastWindowKey {
		ml.lastWindowKey = windowKey
		ml.windowSince = now
	}
	dwellSeconds := now.Sub(ml.windowSince).Seconds()
	sinceInput := secondsSinceLastInput()

	// Pixel capture — prefer DXGI on the primary display (index 0).
	var img *image.RGBA
	var err error
	if ml.displayIdx == 0 && c.dxgi != nil {
		img, err = c.dxgi.captureFrame()
		if err != nil {
			c.logger.Warn("DXGI failed; falling back to GDI",
				"display", ml.displayIdx, "error", err)
		}
	}
	if img == nil {
		img, err = ml.captureGDI()
		if err != nil {
			return nil, fmt.Errorf("GDI capture display %d: %w", ml.displayIdx, err)
		}
	}

	// Sensitive-input masking: black out focused password field before queuing.
	if pwRect, isPassword := queryFocusedPasswordRect(); isPassword {
		maskRect(img, pwRect)
		c.logger.Debug("password field masked", "display", ml.displayIdx)
	}

	frame := &Frame{
		Image:      img,
		CapturedAt: now,
		WindowCtx: WindowContext{
			ProcessName:      result.WindowCtx.ProcessName,
			WindowTitle:      result.WindowCtx.WindowTitle,
			BrowserURL:       result.WindowCtx.BrowserURL,
			FocusedInputRole: result.WindowCtx.FocusedInputRole,
			PID:              result.WindowCtx.PID,
		},
		DisplayIndex:      ml.displayIdx,
		DwellSeconds:      dwellSeconds,
		SecondsSinceInput: sinceInput,
	}

	// Translate the active window's virtual-screen rect into image-local coords.
	// Virtual screen origin for this monitor is ml.mon.rect.Min; the captured
	// image's pixel (0,0) corresponds to that virtual-screen point.
	if winRect := queryActiveWindowRect(); winRect.Dx() > 0 && winRect.Dy() > 0 {
		translated := winRect.Sub(ml.mon.rect.Min)
		imageBounds := img.Bounds()
		clamped := translated.Intersect(imageBounds)
		if clamped.Dx() > 32 && clamped.Dy() > 32 {
			frame.WindowCtx.ActiveWindowRect = clamped
		}
	}

	if err := HashFrame(frame); err != nil {
		c.logger.Debug("pHash failed", "display", ml.displayIdx, "error", err)
	}

	// PERF-6: downscale to queue target dimensions after hashing.
	// pHash must be computed on the full-res image first (done above).
	// This reduces queue peak memory from ~830 MB to ~100 MB.
	DownscaleForQueue(frame)

	return frame, nil
}

// captureGDI captures this monitor's pixels, using cached GDI handles.
func (ml *monitorLoop) captureGDI() (*image.RGBA, error) {
	width := ml.mon.rect.Dx()
	height := ml.mon.rect.Dy()
	if width == 0 || height == 0 {
		return nil, fmt.Errorf("monitor %d has zero dimensions", ml.displayIdx)
	}

	if width != ml.cachedW || height != ml.cachedH {
		ml.releaseHandles()

		display, err := syscall.UTF16PtrFromString("DISPLAY")
		if err != nil {
			return nil, fmt.Errorf("UTF16PtrFromString: %w", err)
		}
		hScreen, _, _ := procCreateDC.Call(uintptr(unsafe.Pointer(display)), 0, 0, 0)
		if hScreen == 0 {
			return nil, fmt.Errorf("CreateDC failed")
		}
		hMem, _, _ := procCreateCompatibleDC.Call(hScreen)
		if hMem == 0 {
			procDeleteDC.Call(hScreen)
			return nil, fmt.Errorf("CreateCompatibleDC failed")
		}
		hBmp, _, _ := procCreateCompatibleBitmap.Call(hScreen, uintptr(width), uintptr(height))
		if hBmp == 0 {
			procDeleteDC.Call(hMem)
			procDeleteDC.Call(hScreen)
			return nil, fmt.Errorf("CreateCompatibleBitmap failed")
		}
		procSelectObject.Call(hMem, hBmp)
		ml.screenDC, ml.memDC, ml.bitmap = hScreen, hMem, hBmp
		ml.cachedW, ml.cachedH = width, height
	}

	ret, _, _ := procBitBlt.Call(
		ml.memDC, 0, 0, uintptr(width), uintptr(height),
		ml.screenDC, uintptr(ml.mon.rect.Min.X), uintptr(ml.mon.rect.Min.Y),
		srccopy,
	)
	if ret == 0 {
		ml.releaseHandles()
		return nil, fmt.Errorf("BitBlt failed")
	}

	bmi := bitmapInfoHeader{
		biSize: 40, biWidth: int32(width), biHeight: -int32(height),
		biPlanes: 1, biBitCount: 32,
	}
	pixels := make([]byte, width*height*4)
	ret, _, _ = procGetDIBits.Call(
		ml.screenDC, ml.bitmap, 0, uintptr(height),
		uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
	)
	if ret == 0 {
		ml.releaseHandles()
		return nil, fmt.Errorf("GetDIBits failed")
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Pix[i*4+0] = pixels[i*4+2]
		img.Pix[i*4+1] = pixels[i*4+1]
		img.Pix[i*4+2] = pixels[i*4+0]
		img.Pix[i*4+3] = 0xFF
	}
	return img, nil
}

// releaseHandles frees the per-monitor GDI objects.
func (ml *monitorLoop) releaseHandles() {
	if ml.bitmap != 0 { procDeleteObject.Call(ml.bitmap); ml.bitmap = 0 }
	if ml.memDC != 0 { procDeleteDC.Call(ml.memDC); ml.memDC = 0 }
	if ml.screenDC != 0 { procDeleteDC.Call(ml.screenDC); ml.screenDC = 0 }
	ml.cachedW, ml.cachedH = 0, 0
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
	if state.url != "" {
		return state.url
	}

	// EnumChildWindows found no Chromium omnibox control. Fall back to
	// IUIAutomation — this handles Firefox and any other accessible browser.
	return queryBrowserURLViaUIA(hwnd)
}


// maskRect blacks out a rectangular region of img described by [left, top, width, height]
// in screen coordinates. Clamps to image bounds.
func maskRect(img *image.RGBA, r [4]float64) {
	if img == nil {
		return
	}
	x0, y0 := int(r[0]), int(r[1])
	x1, y1 := x0+int(r[2]), y0+int(r[3])
	b := img.Bounds()
	if x0 < b.Min.X { x0 = b.Min.X }
	if y0 < b.Min.Y { y0 = b.Min.Y }
	if x1 > b.Max.X { x1 = b.Max.X }
	if y1 > b.Max.Y { y1 = b.Max.Y }
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0, 0, 0, 0xFF
		}
	}
}

// Close releases capture resources.
func (c *WindowsCapturer) Close() error {
	if c.dxgi != nil {
		c.dxgi.Close()
		c.dxgi = nil
	}
	return nil
}

// queryVisibleBackgroundProcessesImpl returns the process names of all visible
// top-level windows that are NOT the current foreground window.
// Uses EnumWindows to walk all top-level HWNDs; skips invisible and the
// foreground window. Only the process name is returned — enough for blocklist matching.
func queryVisibleBackgroundProcessesImpl() []string {
	foreground, _, _ := procGetForegroundWindow.Call()

	var procs []string

	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if hwnd == foreground {
			return 1 // skip foreground — already checked by queryWindowContextImpl
		}
		// IsWindowVisible returns non-zero for visible windows.
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		// Get PID, then process name.
		var pid uint32
		procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid == 0 {
			return 1
		}
		hProc, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
		if hProc == 0 {
			return 1
		}
		defer procCloseHandle.Call(hProc)
		var exeBuf [windows.MAX_PATH]uint16
		size := uint32(len(exeBuf))
		ret, _, _ := procQueryFullProcessImageNameW.Call(hProc, 0,
			uintptr(unsafe.Pointer(&exeBuf[0])), uintptr(unsafe.Pointer(&size)))
		if ret != 0 {
			fullPath := syscall.UTF16ToString(exeBuf[:size])
			procs = append(procs, strings.ToLower(filepath.Base(fullPath)))
		}
		return 1 // continue enumeration
	})

	procEnumWindows.Call(cb, 0)
	return procs
}

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

	// Check whether the focused element is a password field via UIA.
	// When true, the privacy gate blocks the entire frame (Step 3 in privacy.go).
	// This was always "" before, making that gate path permanently dead on Windows.
	focusedInputRole := ""
	if queryFocusedIsPassword() {
		focusedInputRole = "password"
	}

	return windowMetadata{
		ProcessName:      processName,
		WindowTitle:      title,
		BrowserURL:       browserURL,
		FocusedInputRole: focusedInputRole,
		PID:              int(pid),
	}, nil
}
