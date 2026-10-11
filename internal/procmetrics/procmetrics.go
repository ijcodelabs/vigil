// Package procmetrics reports CPU, memory and count for named groups of
// processes: "all the nginx workers", "the JVM whose command line mentions
// order-service".
package procmetrics

import (
	"context"
	"regexp"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

type group struct {
	name, match string
	cmdline     *regexp.Regexp
}

// Collector is built once per config; Collect is called every interval.
type Collector struct {
	groups       []group
	needCmdline  bool
	lastComplain time.Time
}

func New(cfg []config.ProcessGroup) *Collector {
	c := &Collector{}
	for _, g := range cfg {
		if config.Off(g.Enabled) {
			continue
		}
		gr := group{name: g.Name, match: g.Match}
		if g.Cmdline != "" {
			gr.cmdline = regexp.MustCompile(g.Cmdline) // checked when the config was accepted
			c.needCmdline = true
		} else if gr.match == "" {
			gr.match = g.Name
		}
		c.groups = append(c.groups, gr)
	}
	return c
}

// Active reports whether any group is configured.
func (c *Collector) Active() bool { return len(c.groups) > 0 }

type totals struct{ count, cpu, rss, threads float64 }

// Collect returns, per group:
//
//	process_group_count                 processes matched (0 means it is not running)
//	process_group_cpu_seconds_total     CPU time they have used
//	process_group_memory_rss_bytes      resident memory
//	process_group_threads
func (c *Collector) Collect(ctx context.Context) ([]model.Sample, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	sums := make([]totals, len(c.groups))
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue // gone, or not ours to look at
		}
		cmd := ""
		if c.needCmdline {
			cmd, _ = p.CmdlineWithContext(ctx)
		}
		for i, g := range c.groups {
			if (g.cmdline != nil && !g.cmdline.MatchString(cmd)) || (g.cmdline == nil && g.match != name) {
				continue
			}
			t := &sums[i]
			t.count++
			if cpu, err := p.TimesWithContext(ctx); err == nil {
				t.cpu += cpu.User + cpu.System
			}
			if mem, err := p.MemoryInfoWithContext(ctx); err == nil && mem != nil {
				t.rss += float64(mem.RSS)
			}
			if n, err := p.NumThreadsWithContext(ctx); err == nil {
				t.threads += float64(n)
			}
		}
	}
	ts := time.Now().UnixMilli()
	var out []model.Sample
	for i, g := range c.groups {
		for name, v := range map[string]float64{"process_group_count": sums[i].count, "process_group_cpu_seconds_total": sums[i].cpu,
			"process_group_memory_rss_bytes": sums[i].rss, "process_group_threads": sums[i].threads} {
			out = append(out, model.Sample{Name: name, Labels: map[string]string{"group": g.name}, Value: v, TS: ts})
		}
	}
	return out, nil
}
