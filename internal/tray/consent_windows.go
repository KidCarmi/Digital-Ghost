//go:build windows

package tray

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"time"
	"unsafe"

	"fyne.io/systray"
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

// startTrayIconImpl launches the Windows system tray icon using fyne.io/systray.
//
// Blocks until the icon is visible in the taskbar (or returns an error after
// 3 seconds). The icon and its event loop then continue in a background
// goroutine for the lifetime of the process.
//
// Menu:
//
//	Pause Capture   (toggles to Resume Capture when paused)
//	Open Search UI
//	──────────────
//	Stop Digital Ghost
func startTrayIconImpl(onStop, onPause, onResume func()) error {
	ready := make(chan error, 1)

	go func() {
		systray.Run(func() {
			systray.SetIcon(trayIconPNG())
			systray.SetTooltip("Digital Ghost — capturing")

			mPause := systray.AddMenuItem("Pause Capture", "Temporarily pause screen capture")
			mResume := systray.AddMenuItem("Resume Capture", "Resume screen capture")
			mResume.Hide()
			mOpen := systray.AddMenuItem("Open Search UI", "Open the memory search interface in your browser")
			systray.AddSeparator()
			mStop := systray.AddMenuItem("Stop Digital Ghost", "Shut down Digital Ghost")

			ready <- nil // tray is live; unblock startTrayIconImpl

			for {
				select {
				case <-mPause.ClickedCh:
					onPause()
					mPause.Hide()
					mResume.Show()
					systray.SetTooltip("Digital Ghost — PAUSED")

				case <-mResume.ClickedCh:
					onResume()
					mResume.Hide()
					mPause.Show()
					systray.SetTooltip("Digital Ghost — capturing")

				case <-mOpen.ClickedCh:
					// Open the local search UI in the default browser.
					exec.Command("cmd", "/c", "start", "http://localhost:7327").Start() //nolint:errcheck

				case <-mStop.ClickedCh:
					systray.Quit()
					onStop()
					return
				}
			}
		}, func() {
			// onExit — daemon shutdown is handled by onStop callback above.
		})
	}()

	select {
	case err := <-ready:
		return err
	case <-time.After(3 * time.Second):
		return fmt.Errorf("tray icon initialization timed out")
	}
}

// trayIconPNG returns a 32×32 PNG of a filled purple circle — the DG logo.
// Generated programmatically to avoid embedding a binary asset file.
func trayIconPNG() []byte {
	const size = 32
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx, cy := float64(size)/2, float64(size)/2
	r := float64(size)/2 - 1
	purple := color.NRGBA{R: 0x7c, G: 0x3a, B: 0xed, A: 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			if dx*dx+dy*dy <= r*r {
				img.SetNRGBA(x, y, purple)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img) //nolint:errcheck
	return buf.Bytes()
}
