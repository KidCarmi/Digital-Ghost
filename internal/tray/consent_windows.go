//go:build windows

package tray

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK              = 0x00000000
	mbOKCancel        = 0x00000001
	mbYesNo           = 0x00000004
	mbIconInformation = 0x00000040
	mbIconWarning     = 0x00000030
	mbDefButton2      = 0x00000100
	mbSystemModal     = 0x00001000

	idOK  = 1
	idYes = 6
)

func messageBox(caption, text string, style uint32) (int32, error) {
	captionPtr, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return 0, fmt.Errorf("encoding caption: %w", err)
	}
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return 0, fmt.Errorf("encoding text: %w", err)
	}
	ret, _, callErr := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(captionPtr)),
		uintptr(style),
	)
	// MessageBoxW returns 0 only on error (callErr is always non-nil on Windows
	// even on success, so only trust it when ret == 0).
	if ret == 0 {
		return 0, fmt.Errorf("MessageBoxW failed: %w", callErr)
	}
	return int32(ret), nil
}

// showConsentDialogImpl shows a native Windows MessageBox consent dialog.
func showConsentDialogImpl(text string) (bool, error) {
	ret, err := messageBox(
		"Digital Ghost - Consent Required",
		text+"\n\nClick Yes to allow screen capture, or No to decline.",
		mbYesNo|mbIconInformation|mbSystemModal,
	)
	if err != nil {
		return false, fmt.Errorf("showing Windows consent dialog: %w", err)
	}
	return ret == idYes, nil
}

// startTrayIconImpl is a stub on Windows for now.
// Production: use github.com/getlantern/systray or fyne.io/systray.
func startTrayIconImpl(_ func()) error {
	return nil
}
