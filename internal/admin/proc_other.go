//go:build !linux

package admin

// procStats is the same shape as the Linux build, zero-valued on
// non-Linux platforms. SPEC5 §13 commits to Linux-only; this stub
// exists so dev builds on macOS / *BSD still compile. Process
// metrics are zeroed per SPEC5 line 1434 ("Process metrics are
// zeroed; no error surfaces to the scraper").
type procStats struct {
	cpuSeconds          float64
	residentMemoryBytes int64
	virtualMemoryBytes  int64
	openFDs             int64
	maxFDs              int64
}

// readProcStats returns a zero-valued procStats on non-Linux. No
// error is returned because SPEC5 commits to silent zeroing.
func readProcStats() (procStats, error) {
	return procStats{}, nil
}
