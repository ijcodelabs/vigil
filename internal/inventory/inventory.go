// Package inventory finds out what runs on the agent's host: process names,
// listening TCP ports and container images. The server matches this against
// its integrations to say what could be monitored but is not. Only names
// are collected, never command lines.
package inventory

import (
	"context"
	"sort"

	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/ijcodelabs/vigil/internal/model"
)

const maxEach = 1000

func sorted[T int | string](set map[T]bool) []T {
	out := make([]T, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > maxEach {
		out = out[:maxEach]
	}
	return out
}

// Collect returns what can be seen; a part that cannot be read is left
// empty. images, if not nil, lists the images of running containers.
func Collect(ctx context.Context, images func() []string) model.Inventory {
	procs := map[string]bool{}
	if list, err := process.ProcessesWithContext(ctx); err == nil {
		for _, p := range list {
			if name, err := p.NameWithContext(ctx); err == nil && name != "" {
				procs[name] = true
			}
		}
	}
	ports := map[int]bool{}
	if conns, err := net.ConnectionsWithContext(ctx, "tcp"); err == nil {
		for _, c := range conns {
			if c.Status == "LISTEN" && c.Laddr.Port != 0 {
				ports[int(c.Laddr.Port)] = true
			}
		}
	}
	imgs := map[string]bool{}
	if images != nil {
		for _, img := range images() {
			imgs[img] = true
		}
	}
	return model.Inventory{Processes: sorted(procs), Ports: sorted(ports), Images: sorted(imgs)}
}
