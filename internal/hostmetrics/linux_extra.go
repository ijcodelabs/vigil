package hostmetrics

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func procPath(parts ...string) string {
	root := os.Getenv("HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// extra reads the Linux-only figures straight from /proc: memory, CPU and
// I/O pressure, open file descriptors, TCP retransmissions and OOM kills.
// On other systems the files are absent and nothing is added.
func extra(add func(name string, v float64, kv ...string)) {
	// Pressure stall information: share of the last 10 seconds in which
	// work was stalled waiting for the resource.
	for _, res := range []string{"cpu", "memory", "io"} {
		f, err := os.Open(procPath("pressure", res))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text()) // some avg10=0.00 avg60=0.00 avg300=0.00 total=0
			if len(fields) < 2 {
				continue
			}
			for _, kv := range fields[1:] {
				if v, ok := strings.CutPrefix(kv, "avg10="); ok {
					if n, err := strconv.ParseFloat(v, 64); err == nil {
						add("host_pressure_ratio", n/100, "resource", res, "kind", fields[0])
					}
				}
			}
		}
		f.Close()
	}
	// allocated, unused, maximum
	if b, err := os.ReadFile(procPath("sys", "fs", "file-nr")); err == nil {
		if f := strings.Fields(string(b)); len(f) == 3 {
			used, _ := strconv.ParseFloat(f[0], 64)
			limit, _ := strconv.ParseFloat(f[2], 64)
			add("host_file_descriptors", used)
			add("host_file_descriptors_limit", limit)
		}
	}
	// /proc/net/snmp has a header line and a value line per protocol.
	if b, err := os.ReadFile(procPath("net", "snmp")); err == nil {
		lines := strings.Split(string(b), "\n")
		for i := 0; i+1 < len(lines); i++ {
			if !strings.HasPrefix(lines[i], "Tcp:") || !strings.HasPrefix(lines[i+1], "Tcp:") {
				continue
			}
			names, vals := strings.Fields(lines[i]), strings.Fields(lines[i+1])
			for j, n := range names {
				if j >= len(vals) {
					break
				}
				v, err := strconv.ParseFloat(vals[j], 64)
				if err != nil {
					continue
				}
				switch n {
				case "RetransSegs":
					add("host_tcp_retransmits_total", v)
				case "OutSegs":
					add("host_tcp_segments_sent_total", v)
				case "CurrEstab":
					add("host_tcp_connections", v)
				}
			}
			break
		}
	}
	if b, err := os.ReadFile(procPath("vmstat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
				if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					add("host_oom_kills_total", n)
				}
			}
		}
	}
}
