//go:build linux

package admin

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// _SC_CLK_TCK on Linux is conventionally 100 — the kernel's
// CONFIG_HZ default and the value glibc reports for every
// distribution Phase 5 supports. SPEC5 §13 commits to Linux-only;
// we hard-code rather than calling sysconf because cgo'd sysconf
// would force a cgo build. If a future kernel ships HZ=250 the
// CPU seconds reading would scale accordingly — operators would
// see a 2.5x error in the cpu_seconds counter; that is loud
// enough to catch in monitoring without complicating the build.
const linuxClockTicksPerSecond = 100

// procStats represents a single point-in-time read of the SPEC5
// §10.4.7 process-collector inputs from /proc/self/*.
type procStats struct {
	cpuSeconds          float64
	residentMemoryBytes int64
	virtualMemoryBytes  int64
	openFDs             int64
	maxFDs              int64
}

// readProcStats reads /proc/self/stat, /proc/self/statm,
// /proc/self/fd, and getrlimit(RLIMIT_NOFILE). Any individual
// failure is non-fatal — the affected field stays zero — so a
// container without one of these endpoints still emits the
// fields it can read.
func readProcStats() (procStats, error) {
	var p procStats

	// /proc/self/stat: fields utime (14) and stime (15) in clock ticks.
	// Field 2 is "(comm)" which can contain spaces and parentheses;
	// scan past the LAST `)` to land at field 3 (state) and count
	// from there.
	if statBytes, err := os.ReadFile("/proc/self/stat"); err == nil {
		s := string(statBytes)
		rp := strings.LastIndexByte(s, ')')
		if rp < 0 {
			return p, fmt.Errorf("/proc/self/stat: missing ')' delimiter")
		}
		fields := strings.Fields(s[rp+1:])
		// fields[0] = state (orig field 3); orig field 14 = fields[11];
		// orig field 15 = fields[12].
		if len(fields) >= 13 {
			utime, _ := strconv.ParseInt(fields[11], 10, 64)
			stime, _ := strconv.ParseInt(fields[12], 10, 64)
			p.cpuSeconds = float64(utime+stime) / float64(linuxClockTicksPerSecond)
		}
	}

	// /proc/self/statm: fields[0]=size (vsize in pages), fields[1]=resident (RSS in pages).
	if statmBytes, err := os.ReadFile("/proc/self/statm"); err == nil {
		statmFields := strings.Fields(string(statmBytes))
		if len(statmFields) >= 2 {
			pageSize := int64(os.Getpagesize())
			vsizePages, _ := strconv.ParseInt(statmFields[0], 10, 64)
			rssPages, _ := strconv.ParseInt(statmFields[1], 10, 64)
			p.virtualMemoryBytes = vsizePages * pageSize
			p.residentMemoryBytes = rssPages * pageSize
		}
	}

	// /proc/self/fd: count entries.
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		p.openFDs = int64(len(entries))
	}

	// getrlimit(RLIMIT_NOFILE): soft limit.
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil {
		p.maxFDs = int64(rl.Cur)
	}

	return p, nil
}
