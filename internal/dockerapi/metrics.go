package dockerapi

import (
	"context"
	"errors"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
)

// Metrics reads whether each container runs and, for the running ones, CPU,
// memory and network usage. A container that stopped is still reported
// (container_running 0, with its exit code) until it is removed, which is
// what lets a rule tell "crashed" from "gone on purpose".
func (c *Client) Metrics(ctx context.Context) ([]model.Sample, error) {
	containers, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Sample
	var lastErr error
	for _, ct := range containers {
		// Never started, or on its way out: neither up nor down.
		if ct.State == "created" || ct.State == "removing" {
			continue
		}
		ts := time.Now().UnixMilli()
		add := func(name string, v float64, kv ...string) {
			l := map[string]string{"container": ct.Name(), "image": ct.Image}
			for i := 0; i+1 < len(kv); i += 2 {
				l[kv[i]] = kv[i+1]
			}
			out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
		}
		rs, err := c.RunState(ctx, ct.ID)
		if errors.Is(err, ErrGone) {
			continue // it ended since the list was taken
		}
		if err != nil {
			lastErr = err
			continue
		}
		add("container_restarts_total", float64(rs.RestartCount))
		if !rs.Running {
			add("container_running", 0)
			add("container_exit_code", float64(rs.ExitCode))
			continue
		}
		add("container_running", 1)
		st, err := c.Stats(ctx, ct.ID)
		if errors.Is(err, ErrGone) {
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		// Page cache that can be reclaimed is not counted as used, as `docker stats` does.
		// cgroup v2 calls it inactive_file, v1 total_inactive_file.
		cache := st.Memory.Stats["inactive_file"]
		if v, ok := st.Memory.Stats["total_inactive_file"]; ok {
			cache = v
		}
		used := st.Memory.Usage
		if cache < used {
			used -= cache
		}
		add("container_cpu_usage_seconds_total", float64(st.CPU.Usage.Total)/1e9)
		add("container_memory_usage_bytes", float64(used))
		add("container_memory_limit_bytes", float64(st.Memory.Limit))
		for nic, n := range st.Networks {
			add("container_network_receive_bytes_total", float64(n.RxBytes), "interface", nic)
			add("container_network_transmit_bytes_total", float64(n.TxBytes), "interface", nic)
		}
	}
	return out, lastErr
}
