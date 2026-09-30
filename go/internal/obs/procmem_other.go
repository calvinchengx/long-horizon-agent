//go:build !unix

package obs

// PeakRSSMB is 0 where getrusage is unavailable (python: peak_rss_mb).
func PeakRSSMB() float64 { return 0 }
