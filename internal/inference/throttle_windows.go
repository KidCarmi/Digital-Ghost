//go:build windows

package inference

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	modKernel32       = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes = modKernel32.NewProc("GetSystemTimes")
)

// winFILETIME mirrors the Win32 FILETIME structure (100-nanosecond intervals
// since January 1, 1601). Used to read values from GetSystemTimes.
type winFILETIME struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func fileTimeToInt64(ft winFILETIME) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// readCPURaw returns cumulative (total, idle) in 100-ns intervals using the
// Win32 GetSystemTimes API.
//
// GetSystemTimes returns three values:
//   - IdleTime   — time all CPUs spent idle
//   - KernelTime — time in kernel mode (includes IdleTime)
//   - UserTime   — time in user mode
//
// total = KernelTime + UserTime   (KernelTime already includes idle)
// idle  = IdleTime
//
// The delta math in updateMetrics (caller) then gives:
//   busy% = 100 × (Δtotal − Δidle) / Δtotal
//
// Returns (0, 0) on error; the caller handles the zero-value gracefully.
func readCPURaw() (total, idle int64) {
	var idleTime, kernelTime, userTime winFILETIME
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if r == 0 {
		return 0, 0
	}
	k := fileTimeToInt64(kernelTime)
	u := fileTimeToInt64(userTime)
	i := fileTimeToInt64(idleTime)
	return k + u, i
}

// readGPUPercent reads GPU utilization via nvidia-smi on Windows.
// Intel/AMD sysfs paths are Linux-only; on Windows those drivers don't expose
// a sysfs interface so we skip straight to nvidia-smi.
// Returns 0 if GPU metrics are unavailable — conservative (allows inference).
func readGPUPercent() int {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu",
		"--format=csv,noheader,nounits",
	).Output()
	if err == nil {
		// Output: "<util>\n" or "<util1>\n<util2>\n" for multi-GPU.
		line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		if v, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			return v
		}
	}
	// No GPU metrics available — return 0 (conservative: allow inference).
	return 0
}
