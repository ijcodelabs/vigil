//go:build !unix

package agent

func cpuSeconds() (float64, bool)    { return 0, false }
func residentBytes() (float64, bool) { return 0, false }
func openFiles() (float64, bool)     { return 0, false }
