//go:build unix

package agent

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// cpuSeconds is the processor time this process has used, user and system.
func cpuSeconds() (float64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	sec := func(tv syscall.Timeval) float64 { return float64(tv.Sec) + float64(tv.Usec)/1e6 }
	return sec(ru.Utime) + sec(ru.Stime), true
}

// residentBytes is the memory the kernel holds for this process. The
// agent's own /proc entry is read, not the host's: in a container that is
// the agent itself. Only Linux has it.
func residentBytes() (float64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseFloat(f[1], 64)
	return pages * float64(os.Getpagesize()), err == nil
}

// openFiles counts this process's open file descriptors (Linux).
func openFiles() (float64, bool) {
	d, err := os.Open("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	return float64(len(names) - 1), err == nil // without the one used for looking
}
