//go:build unix

package obs

import (
	"math"
	"runtime"
	"syscall"
)

// PeakRSSMB is the process's peak resident set size in MiB (one decimal; python: peak_rss_mb).
// A structure that grows with every cycle shows up as a peak that climbs checkpoint after
// checkpoint, without a profiler.
func PeakRSSMB() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	scale := 1024.0 // Linux reports KiB
	if runtime.GOOS == "darwin" {
		scale = 1 // bytes
	}
	return math.Round(float64(ru.Maxrss)*scale/(1<<20)*10) / 10
}
