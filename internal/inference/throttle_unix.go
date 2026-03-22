//go:build !windows

package inference

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// readCPURaw returns the cumulative (total, idle) jiffies from /proc/stat.
// Returns (0, 0) on error; the caller handles the zero-value gracefully.
func readCPURaw() (total, idle int64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line := strings.SplitN(string(data), "\n", 2)[0] // first line: "cpu  ..."
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}

	// Fields: user, nice, system, idle, iowait, irq, softirq, ...
	for i, f := range fields[1:] {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, 0
		}
		total += v
		if i == 3 { // idle field
			idle = v
		}
	}
	return total, idle
}

// readGPUPercent reads GPU utilization via sysfs (Intel/AMD) or nvidia-smi.
// Returns 0 if GPU metrics are unavailable — conservative (allows inference).
func readGPUPercent() int {
	// Intel integrated GPU via sysfs.
	for _, p := range []string{
		"/sys/class/drm/card0/gt/gt0/busy_percent",
		"/sys/class/drm/card1/gt/gt0/busy_percent",
	} {
		if data, err := os.ReadFile(p); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return v
			}
		}
	}

	// AMD via amdgpu sysfs.
	if data, err := os.ReadFile("/sys/class/drm/card0/device/gpu_busy_percent"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			return v
		}
	}

	// NVIDIA via nvidia-smi — time-boxed so a missing driver doesn't stall.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu",
		"--format=csv,noheader,nounits",
	).Output()
	if err == nil {
		line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		if v, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			return v
		}
	}

	return 0
}
