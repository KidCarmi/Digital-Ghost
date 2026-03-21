//go:build windows

// dxgi_windows.go implements screen capture via the DXGI Desktop Duplication API.
//
// This is the GPU-accelerated alternative to the GDI BitBlt path in
// capture_windows.go. It produces the same output (*image.RGBA, top-down,
// RGBA byte order) but runs entirely through the GPU compositor, which
// eliminates the CPU-side BitBlt blit and the GDI device context creation
// overhead on every frame.
//
// API path:
//
//	D3D11CreateDevice()
//	  → ID3D11Device + ID3D11DeviceContext
//	  → QueryInterface(IID_IDXGIDevice) → IDXGIDevice
//	  → IDXGIDevice.GetAdapter → IDXGIAdapter
//	  → IDXGIAdapter.EnumOutputs(i) → IDXGIOutput
//	  → QueryInterface(IID_IDXGIOutput1) → IDXGIOutput1
//	  → IDXGIOutput1.DuplicateOutput → IDXGIOutputDuplication
//
// Per-frame:
//
//	IDXGIOutputDuplication.AcquireNextFrame(timeout, &info, &pResource)
//	  → QueryInterface(IID_ID3D11Texture2D) → ID3D11Texture2D pSrc
//	  → pDevice.CreateTexture2D(desc_staging, nil, &pStaging)
//	  → pCtx.CopyResource(pStaging, pSrc)
//	  → pCtx.Map(pStaging, 0, D3D11_MAP_READ, 0, &mapped)
//	  → copy bytes
//	  → pCtx.Unmap(pStaging, 0)
//	  → IDXGIOutputDuplication.ReleaseFrame()
//
// Error handling:
//
//	DXGI_ERROR_ACCESS_LOST (0x887A0026): screen locked or session changed.
//	  Release the duplication interface and re-create it before the next frame.
//	DXGI_ERROR_WAIT_TIMEOUT (0x887A0027): no new frame within timeout.
//	  Not an error — the screen didn't change; skip this tick.
//
// All COM pointers are released explicitly. The OS thread is locked for the
// entire lifetime of the DXGI session (runtime.LockOSThread / UnlockOSThread).
//
// No CGo required. All COM calls go through raw vtable calls via syscall.SyscallN.
package capture

import (
	"fmt"
	"image"
	"log/slog"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// -- COM GUIDs for DXGI / D3D11 ------------------------------------------

var (
	iidIDXGIDevice = windows.GUID{
		Data1: 0x54ec77fa, Data2: 0x1377, Data3: 0x44e6,
		Data4: [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c},
	}
	iidIDXGIAdapter = windows.GUID{
		Data1: 0x2411e7e1, Data2: 0x12ac, Data3: 0x4ccf,
		Data4: [8]byte{0xbd, 0x14, 0x97, 0x98, 0xe8, 0x53, 0x4d, 0xc0},
	}
	iidIDXGIOutput1 = windows.GUID{
		Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83,
		Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc},
	}
	iidID3D11Texture2D = windows.GUID{
		Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89,
		Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c},
	}
)

// -- DLL / proc refs -------------------------------------------------------

var (
	d3d11DLL            = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice = d3d11DLL.NewProc("D3D11CreateDevice")
)

// -- D3D11 / DXGI constants -----------------------------------------------

const (
	d3dDriverTypeHardware  = 1    // D3D_DRIVER_TYPE_HARDWARE
	d3d11SDKVersion        = 7    // D3D11_SDK_VERSION
	dxgiFormatB8G8R8A8Unorm = 87 // DXGI_FORMAT_B8G8R8A8_UNORM
	d3d11UsageStaging      = 3    // D3D11_USAGE_STAGING
	d3d11CPUAccessRead     = 0x20000
	d3d11MapRead           = 1    // D3D11_MAP_READ
	dxgiErrorAccessLost    = uintptr(0x887A0026)
	dxgiErrorWaitTimeout   = uintptr(0x887A0027)
)

// -- D3D11 vtable slot indices ---------------------------------------------
//
// Slots are absolute 0-indexed positions in the vtable (IUnknown occupies 0-2).
// IUnknown: 0=QueryInterface, 1=AddRef, 2=Release
// ID3D11DeviceChild: 3=GetDevice, 4=GetPrivateData, 5=SetPrivateData, 6=SetPrivateDataInterface
// ID3D11Device (base at 3, own methods start at 3 after IUnknown):
//   3=CreateBuffer, 4=CreateTexture1D, 5=CreateTexture2D, ...
// ID3D11DeviceContext (inherits ID3D11DeviceChild, own methods start at 7):
//   7=VSSetConstantBuffers, ..., 14=Map, 15=Unmap, ..., 36=CopyResource

const (
	d3d11DeviceSlotCreateTexture2D      = 5
	d3d11DeviceCtxSlotMap               = 14
	d3d11DeviceCtxSlotUnmap             = 15
	d3d11DeviceCtxSlotCopyResource      = 36
)

// -- IDXGIDevice vtable slots (inherits IDXGIObject at 3-6) ---------------
//
// IDXGIObject: 3=SetPrivateData, 4=SetPrivateDataInterface, 5=GetPrivateData, 6=GetParent
// IDXGIDevice own: 7=GetAdapter, 8=CreateSurface, ...

const dxgiDeviceSlotGetAdapter = 7

// -- IDXGIAdapter vtable slots --------------------------------------------
//
// IDXGIObject: 3-6
// IDXGIAdapter: 7=EnumOutputs, 8=GetDesc, 9=CheckInterfaceSupport

const dxgiAdapterSlotEnumOutputs = 7

// -- IDXGIOutput1 vtable slots --------------------------------------------
//
// IDXGIObject: 3-6
// IDXGIOutput:  7=GetDesc, 8=GetDisplayModeList, 9=FindClosestMatchingMode,
//               10=WaitForVBlank, 11=TakeOwnership, 12=ReleaseOwnership,
//               13=GetGammaControlCapabilities, 14=SetGammaControl,
//               15=GetGammaControl, 16=SetDisplaySurface,
//               17=GetDisplaySurfaceData, 18=GetFrameStatistics
// IDXGIOutput1: 19=GetDisplayModeList1, 20=FindClosestMatchingMode1,
//               21=GetDisplaySurfaceData1, 22=DuplicateOutput

const dxgiOutput1SlotDuplicateOutput = 22

// -- IDXGIOutputDuplication vtable slots ----------------------------------
//
// IDXGIObject: 3-6
// IDXGIOutputDuplication: 7=GetDesc, 8=AcquireNextFrame, 9=GetFrameDirtyRects,
//   10=GetFrameMoveRects, 11=GetFramePointerShape, 12=MapDesktopSurface,
//   13=UnMapDesktopSurface, 14=ReleaseFrame

const (
	dxgiDuplSlotAcquireNextFrame = 8
	dxgiDuplSlotReleaseFrame     = 14
)

// -- Win64 structures -------------------------------------------------------

// d3d11Texture2DDesc mirrors D3D11_TEXTURE2D_DESC.
type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleDescCount  uint32
	SampleDescQuality uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

// d3d11MappedSubresource mirrors D3D11_MAPPED_SUBRESOURCE.
type d3d11MappedSubresource struct {
	pData     uintptr
	RowPitch  uint32
	DepthPitch uint32
}

// dxgiOutduplFrameInfo mirrors DXGI_OUTDUPL_FRAME_INFO (simplified; we only
// need it to satisfy the AcquireNextFrame call signature — we don't read fields).
type dxgiOutduplFrameInfo struct {
	LastPresentTime         int64
	LastMouseUpdateTime     int64
	AccumulatedFrames       uint32
	RectsCoalesced          uint32
	ProtectedContentMaskedOut uint32
	PointerPositionX        int32
	PointerPositionY        int32
	PointerPositionVisible  uint32
	TotalMetadataBufferSize uint32
	PointerShapeBufferSize  uint32
}

// -- DXGISession holds all COM state for a single-monitor DXGI session ----

// DXGISession holds the D3D11 and DXGI COM pointers needed to capture one monitor.
// It is created by OpenDXGISession and must be closed when no longer needed.
type DXGISession struct {
	pDevice    uintptr // ID3D11Device
	pCtx       uintptr // ID3D11DeviceContext
	pDupl      uintptr // IDXGIOutputDuplication
	width      int
	height     int
	logger     *slog.Logger
}

// OpenDXGISession creates a D3D11 device and acquires a desktop duplication
// interface for the monitor at outputIndex on the primary adapter.
// The caller's goroutine must have its OS thread locked (runtime.LockOSThread).
func OpenDXGISession(outputIndex int, logger *slog.Logger) (*DXGISession, error) {
	var pDevice, pCtx uintptr

	hr, _, _ := procD3D11CreateDevice.Call(
		0,                           // pAdapter (nil = use default adapter)
		d3dDriverTypeHardware,       // DriverType
		0,                           // Software (null for hardware)
		0,                           // Flags
		0,                           // pFeatureLevels (null = use default set)
		0,                           // FeatureLevels
		d3d11SDKVersion,             // SDKVersion
		uintptr(unsafe.Pointer(&pDevice)),
		0,                           // pFeatureLevel (we don't care)
		uintptr(unsafe.Pointer(&pCtx)),
	)
	if hr != 0 || pDevice == 0 || pCtx == 0 {
		return nil, fmt.Errorf("D3D11CreateDevice failed: HRESULT=0x%08x", hr)
	}

	// QueryInterface(ID3D11Device, IID_IDXGIDevice, &pDXGIDevice)
	var pDXGIDevice uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pDevice, 0), // QueryInterface
		pDevice,
		uintptr(unsafe.Pointer(&iidIDXGIDevice)),
		uintptr(unsafe.Pointer(&pDXGIDevice)),
	)
	if hr != 0 || pDXGIDevice == 0 {
		dxgiRelease(pCtx)
		dxgiRelease(pDevice)
		return nil, fmt.Errorf("QueryInterface(IDXGIDevice) failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pDXGIDevice)

	// IDXGIDevice.GetAdapter(&pAdapter)
	var pAdapter uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pDXGIDevice, dxgiDeviceSlotGetAdapter),
		pDXGIDevice,
		uintptr(unsafe.Pointer(&pAdapter)),
	)
	if hr != 0 || pAdapter == 0 {
		dxgiRelease(pCtx)
		dxgiRelease(pDevice)
		return nil, fmt.Errorf("IDXGIDevice.GetAdapter failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pAdapter)

	// IDXGIAdapter.EnumOutputs(outputIndex, &pOutput)
	var pOutput uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pAdapter, dxgiAdapterSlotEnumOutputs),
		pAdapter,
		uintptr(outputIndex),
		uintptr(unsafe.Pointer(&pOutput)),
	)
	if hr != 0 || pOutput == 0 {
		dxgiRelease(pCtx)
		dxgiRelease(pDevice)
		return nil, fmt.Errorf("IDXGIAdapter.EnumOutputs(%d) failed: HRESULT=0x%08x", outputIndex, hr)
	}
	defer dxgiRelease(pOutput)

	// QueryInterface(IDXGIOutput, IID_IDXGIOutput1, &pOutput1)
	var pOutput1 uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pOutput, 0), // QueryInterface
		pOutput,
		uintptr(unsafe.Pointer(&iidIDXGIOutput1)),
		uintptr(unsafe.Pointer(&pOutput1)),
	)
	if hr != 0 || pOutput1 == 0 {
		dxgiRelease(pCtx)
		dxgiRelease(pDevice)
		return nil, fmt.Errorf("QueryInterface(IDXGIOutput1) failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pOutput1)

	// IDXGIOutput1.DuplicateOutput(pDevice, &pDupl)
	var pDupl uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pOutput1, dxgiOutput1SlotDuplicateOutput),
		pOutput1,
		pDevice,
		uintptr(unsafe.Pointer(&pDupl)),
	)
	if hr != 0 || pDupl == 0 {
		dxgiRelease(pCtx)
		dxgiRelease(pDevice)
		return nil, fmt.Errorf("IDXGIOutput1.DuplicateOutput failed: HRESULT=0x%08x", hr)
	}

	// Get output dimensions from DXGI_OUTPUT_DESC.
	// We use GetSystemMetrics as a simpler fallback — the desc requires
	// reading a DXGI_OUTPUT_DESC struct which varies in layout.
	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)

	return &DXGISession{
		pDevice: pDevice,
		pCtx:    pCtx,
		pDupl:   pDupl,
		width:   int(w),
		height:  int(h),
		logger:  logger,
	}, nil
}

// CaptureFrame acquires one desktop frame and returns it as *image.RGBA.
//
// Returns:
//   - (img, nil)  — success
//   - (nil, nil)  — no new frame within timeout (DXGI_ERROR_WAIT_TIMEOUT); caller should skip
//   - (nil, errAccessLost) — desktop access lost; caller must call Close() and OpenDXGISession() again
//   - (nil, err)  — other error
var errAccessLost = fmt.Errorf("DXGI desktop access lost (screen lock or session change)")

func (s *DXGISession) CaptureFrame() (*image.RGBA, error) {
	// AcquireNextFrame(timeout=100ms, &frameInfo, &pResource)
	var frameInfo dxgiOutduplFrameInfo
	var pResource uintptr
	hr, _, _ := syscall.SyscallN(
		dxgiVtblSlot(s.pDupl, dxgiDuplSlotAcquireNextFrame),
		s.pDupl,
		100, // timeout ms
		uintptr(unsafe.Pointer(&frameInfo)),
		uintptr(unsafe.Pointer(&pResource)),
	)
	if hr == dxgiErrorWaitTimeout {
		return nil, nil // no new frame; screen unchanged
	}
	if hr == dxgiErrorAccessLost {
		return nil, errAccessLost
	}
	if hr != 0 || pResource == 0 {
		return nil, fmt.Errorf("AcquireNextFrame failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pResource)
	defer s.releaseFrame()

	// QueryInterface(IDXGIResource, IID_ID3D11Texture2D, &pSrc)
	var pSrc uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(pResource, 0), // QueryInterface
		pResource,
		uintptr(unsafe.Pointer(&iidID3D11Texture2D)),
		uintptr(unsafe.Pointer(&pSrc)),
	)
	if hr != 0 || pSrc == 0 {
		return nil, fmt.Errorf("QueryInterface(ID3D11Texture2D) failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pSrc)

	// Create a CPU-readable staging texture with the same dimensions and format.
	stagingDesc := d3d11Texture2DDesc{
		Width:             uint32(s.width),
		Height:            uint32(s.height),
		MipLevels:         1,
		ArraySize:         1,
		Format:            dxgiFormatB8G8R8A8Unorm,
		SampleDescCount:   1,
		SampleDescQuality: 0,
		Usage:             d3d11UsageStaging,
		BindFlags:         0,
		CPUAccessFlags:    d3d11CPUAccessRead,
		MiscFlags:         0,
	}
	var pStaging uintptr
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(s.pDevice, d3d11DeviceSlotCreateTexture2D),
		s.pDevice,
		uintptr(unsafe.Pointer(&stagingDesc)),
		0, // no initial data
		uintptr(unsafe.Pointer(&pStaging)),
	)
	if hr != 0 || pStaging == 0 {
		return nil, fmt.Errorf("CreateTexture2D(staging) failed: HRESULT=0x%08x", hr)
	}
	defer dxgiRelease(pStaging)

	// CopyResource: GPU copies the desktop texture into our staging texture.
	syscall.SyscallN(
		dxgiVtblSlot(s.pCtx, d3d11DeviceCtxSlotCopyResource),
		s.pCtx,
		pStaging,
		pSrc,
	)

	// Map the staging texture for CPU read.
	var mapped d3d11MappedSubresource
	hr, _, _ = syscall.SyscallN(
		dxgiVtblSlot(s.pCtx, d3d11DeviceCtxSlotMap),
		s.pCtx,
		pStaging,
		0,             // Subresource
		d3d11MapRead,  // MapType
		0,             // MapFlags
		uintptr(unsafe.Pointer(&mapped)),
	)
	if hr != 0 || mapped.pData == 0 {
		return nil, fmt.Errorf("ID3D11DeviceContext.Map failed: HRESULT=0x%08x", hr)
	}
	defer syscall.SyscallN(
		dxgiVtblSlot(s.pCtx, d3d11DeviceCtxSlotUnmap),
		s.pCtx, pStaging, 0,
	)

	// Copy pixel data from GPU memory into a Go slice.
	// DXGI gives BGRA; image.RGBA expects RGBA — swap R and B.
	img := image.NewRGBA(image.Rect(0, 0, s.width, s.height))
	rowPitch := int(mapped.RowPitch)
	for y := 0; y < s.height; y++ {
		srcRow := mapped.pData + uintptr(y*rowPitch)
		dstRow := y * img.Stride
		for x := 0; x < s.width; x++ {
			b := *(*byte)(unsafe.Pointer(srcRow + uintptr(x*4+0)))
			g := *(*byte)(unsafe.Pointer(srcRow + uintptr(x*4+1)))
			r := *(*byte)(unsafe.Pointer(srcRow + uintptr(x*4+2)))
			img.Pix[dstRow+x*4+0] = r
			img.Pix[dstRow+x*4+1] = g
			img.Pix[dstRow+x*4+2] = b
			img.Pix[dstRow+x*4+3] = 0xFF
		}
	}
	return img, nil
}

func (s *DXGISession) releaseFrame() {
	syscall.SyscallN(dxgiVtblSlot(s.pDupl, dxgiDuplSlotReleaseFrame), s.pDupl)
}

// Close releases all COM interfaces held by this session.
func (s *DXGISession) Close() {
	if s.pDupl != 0 {
		dxgiRelease(s.pDupl)
		s.pDupl = 0
	}
	if s.pCtx != 0 {
		dxgiRelease(s.pCtx)
		s.pCtx = 0
	}
	if s.pDevice != 0 {
		dxgiRelease(s.pDevice)
		s.pDevice = 0
	}
}

// -- DXGICapturer wraps DXGISession for use in the capture loop -----------

// DXGICapturer captures frames via DXGI on a dedicated OS thread.
type DXGICapturer struct {
	base    *WindowsCapturer // reuse window-metadata and gate logic
	session *DXGISession
	logger  *slog.Logger
}

// NewDXGICapturer opens a DXGI session and returns a DXGICapturer.
// Must be called on an OS-locked goroutine, or will lock the current thread.
func NewDXGICapturer(base *WindowsCapturer, logger *slog.Logger) (*DXGICapturer, error) {
	runtime.LockOSThread()
	session, err := OpenDXGISession(0, logger)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("opening DXGI session: %w", err)
	}
	return &DXGICapturer{base: base, session: session, logger: logger}, nil
}

// captureDXGIFrame is called from WindowsCapturer.captureFrame when the DXGI
// backend is active. It replaces the captureMonitorGDI call.
func (c *DXGICapturer) captureFrame() (*image.RGBA, error) {
	img, err := c.session.CaptureFrame()
	if err == errAccessLost {
		c.logger.Warn("DXGI access lost; reinitialising duplication interface")
		c.session.Close()
		newSession, rerr := OpenDXGISession(0, c.logger)
		if rerr != nil {
			return nil, fmt.Errorf("DXGI reinit failed: %w", rerr)
		}
		c.session = newSession
		return nil, nil // skip this frame; next tick will succeed
	}
	return img, err
}

// Close releases DXGI resources and unlocks the OS thread.
func (c *DXGICapturer) Close() {
	c.session.Close()
	runtime.UnlockOSThread()
}

// -- vtable helpers (same pattern as uia_windows.go) ----------------------

func dxgiVtblSlot(p uintptr, n int) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(p))
	return *(*uintptr)(unsafe.Pointer(vtbl + uintptr(n)*8))
}

func dxgiRelease(p uintptr) {
	if p == 0 {
		return
	}
	syscall.SyscallN(dxgiVtblSlot(p, 2), p) // IUnknown.Release = slot 2
}
