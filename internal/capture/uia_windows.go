//go:build windows

// uia_windows.go implements browser URL extraction via the Windows UI Automation
// (IUIAutomation) COM API.
//
// This is the correct approach for all accessible browsers, including Firefox,
// whose address bar has no Win32 class name reachable by EnumChildWindows.
// It supplements the Chromium class-name scan in capture_windows.go by serving
// as a fallback when that scan finds nothing.
//
// COM is accessed via raw vtable calls using syscall.SyscallN — no CGo needed.
// All COM pointers are released before the function returns.
// Returns "" on any error — URL extraction is best-effort.
//
// Known address-bar AutomationIds:
//
//	Firefox 57+      "urlbar-input"
//	Edge (Chromium)  "addressEditBox"
package capture

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// -- COM GUIDs ---------------------------------------------------------------

var (
	clsidCUIAutomation = windows.GUID{
		Data1: 0xff48dba4, Data2: 0x60ef, Data3: 0x4201,
		Data4: [8]byte{0xaa, 0x87, 0x54, 0x10, 0x3e, 0xef, 0x59, 0x4e},
	}
	iidIUIAutomation = windows.GUID{
		Data1: 0x30cbe57d, Data2: 0xd9d0, Data3: 0x452a,
		Data4: [8]byte{0xab, 0x13, 0x7a, 0xc5, 0xac, 0x48, 0x25, 0xee},
	}
)

// -- UI Automation vtable slot indices (from UIAutomationClient.h) -----------
//
// IUIAutomation (slots are 0-indexed; 0-2 are IUnknown):
//   6  = ElementFromHandle
//   16 = CreatePropertyCondition
//
// IUIAutomationElement:
//   5  = FindFirst
//   10 = GetCurrentPropertyValue

const (
	uiaSlotElementFromHandle      = 6
	uiaSlotCreatePropertyCondition = 16
	uiaElemSlotFindFirst           = 5
	uiaElemSlotGetCurrentPropVal   = 10
	iUnknownSlotRelease            = 2
)

// -- UI Automation property/scope constants ----------------------------------

const (
	uiaAutomationIdPropertyId = 10011 // UIA_AutomationIdPropertyId
	uiaValueValuePropertyId   = 30045 // UIA_ValueValuePropertyId
	treeScopeDescendants      = 4     // TreeScope_Descendants
	vtBSTR                    = 8     // VARIANT vt: BSTR
)

// -- COM procedure stubs -----------------------------------------------------

var (
	procCoInitializeEx   = windows.NewLazySystemDLL("ole32.dll").NewProc("CoInitializeEx")
	procCoUninitialize   = windows.NewLazySystemDLL("ole32.dll").NewProc("CoUninitialize")
	procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")
	procSysAllocString   = windows.NewLazySystemDLL("oleaut32.dll").NewProc("SysAllocString")
	procSysFreeString    = windows.NewLazySystemDLL("oleaut32.dll").NewProc("SysFreeString")
	procVariantClear     = windows.NewLazySystemDLL("oleaut32.dll").NewProc("VariantClear")
)

// -- VARIANT (16 bytes on Win64) ---------------------------------------------

// comVariant mirrors Win32 VARIANT. We only use VT_BSTR (vt=8, data=BSTR ptr).
type comVariant struct {
	vt   uint16
	res1 uint16
	res2 uint16
	res3 uint16
	data uintptr // BSTR pointer (or scalar value for other vt types)
	hi   uintptr // high word — used by DECIMAL; zero for VT_BSTR
}

// -- Public API --------------------------------------------------------------

// queryBrowserURLViaUIA reads the URL currently shown in the browser address
// bar by walking the Windows UI Automation tree. Returns "" on any failure.
//
// Called from extractBrowserURL as a fallback for browsers (primarily Firefox)
// whose address bar cannot be found via Win32 class-name enumeration.
func queryBrowserURLViaUIA(hwnd uintptr) string {
	// Initialize COM on this goroutine's OS thread.
	// 0x0 = COINIT_MULTITHREADED. Return values:
	//   S_OK    (0x0) = first init on this thread — we own uninit
	//   S_FALSE (0x1) = already initialized with same model — skip uninit
	//   Any other HRESULT = failure, abort
	hr, _, _ := procCoInitializeEx.Call(0, 0x0)
	if hr != 0 && hr != 1 {
		return ""
	}
	if hr == 0 {
		defer procCoUninitialize.Call()
	}

	// CoCreateInstance(CLSID_CUIAutomation, nil, CLSCTX_INPROC_SERVER, IID_IUIAutomation, &pUIA)
	var pUIA uintptr
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidCUIAutomation)),
		0,
		0x1, // CLSCTX_INPROC_SERVER
		uintptr(unsafe.Pointer(&iidIUIAutomation)),
		uintptr(unsafe.Pointer(&pUIA)),
	)
	if hr != 0 || pUIA == 0 {
		return ""
	}
	defer uiaRelease(pUIA)

	// IUIAutomation.ElementFromHandle(hwnd, &pRoot)
	var pRoot uintptr
	hr, _, _ = syscall.SyscallN(
		uiaVtblSlot(pUIA, uiaSlotElementFromHandle),
		pUIA, hwnd, uintptr(unsafe.Pointer(&pRoot)),
	)
	if hr != 0 || pRoot == 0 {
		return ""
	}
	defer uiaRelease(pRoot)

	// Try known AutomationIds for browser address bars.
	for _, autoID := range []string{
		"urlbar-input",  // Firefox 57+
		"addressEditBox", // Edge (Chromium)
	} {
		if url := uiaFindValueByAutoID(pUIA, pRoot, autoID); url != "" {
			return url
		}
	}
	return ""
}

// uiaFindValueByAutoID searches pRoot's descendants for an element whose
// AutomationId matches autoID and returns its Value property (the URL text).
func uiaFindValueByAutoID(pUIA, pRoot uintptr, autoID string) string {
	// SysAllocString converts the AutomationId Go string to a BSTR.
	u16, err := syscall.UTF16PtrFromString(autoID)
	if err != nil {
		return ""
	}
	bstr, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(u16)))
	if bstr == 0 {
		return ""
	}
	defer procSysFreeString.Call(bstr)

	// Build VARIANT{vt=VT_BSTR, data=bstr} for CreatePropertyCondition.
	// On Win64, structs > 8 bytes are passed by pointer at the machine level.
	v := comVariant{vt: vtBSTR, data: bstr}

	// IUIAutomation.CreatePropertyCondition(UIA_AutomationIdPropertyId, VARIANT, &pCond)
	var pCond uintptr
	hr, _, _ := syscall.SyscallN(
		uiaVtblSlot(pUIA, uiaSlotCreatePropertyCondition),
		pUIA,
		uiaAutomationIdPropertyId,
		uintptr(unsafe.Pointer(&v)),
		uintptr(unsafe.Pointer(&pCond)),
	)
	if hr != 0 || pCond == 0 {
		return ""
	}
	defer uiaRelease(pCond)

	// IUIAutomationElement.FindFirst(TreeScope_Descendants, pCond, &pFound)
	var pFound uintptr
	hr, _, _ = syscall.SyscallN(
		uiaVtblSlot(pRoot, uiaElemSlotFindFirst),
		pRoot,
		treeScopeDescendants,
		pCond,
		uintptr(unsafe.Pointer(&pFound)),
	)
	if hr != 0 || pFound == 0 {
		return ""
	}
	defer uiaRelease(pFound)

	// IUIAutomationElement.GetCurrentPropertyValue(UIA_ValueValuePropertyId, &retVal)
	var retVal comVariant
	hr, _, _ = syscall.SyscallN(
		uiaVtblSlot(pFound, uiaElemSlotGetCurrentPropVal),
		pFound,
		uiaValueValuePropertyId,
		uintptr(unsafe.Pointer(&retVal)),
	)
	if hr != 0 {
		return ""
	}
	defer procVariantClear.Call(uintptr(unsafe.Pointer(&retVal)))

	if retVal.vt != vtBSTR || retVal.data == 0 {
		return ""
	}
	// BSTR: the pointer addresses the first UTF-16 code unit; read until null.
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(retVal.data)))
}

// -- helpers -----------------------------------------------------------------

// uiaVtblSlot returns the function pointer at vtable slot n for a COM object p.
// Every COM object starts with a pointer to its vtable (an array of function ptrs).
func uiaVtblSlot(p uintptr, n int) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(p))                // dereference COM ptr → vtable ptr
	return *(*uintptr)(unsafe.Pointer(vtbl + uintptr(n)*8)) // index into vtable (8 bytes/slot on 64-bit)
}

// uiaRelease calls IUnknown.Release (vtable slot 2) on a COM pointer.
func uiaRelease(p uintptr) {
	if p == 0 {
		return
	}
	syscall.SyscallN(uiaVtblSlot(p, iUnknownSlotRelease), p) //nolint:errcheck
}
