package agent

import (
	"context"
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

// limiter keeps one host from flooding the stores: a cap on distinct metric
// series, and a rate for log bytes. What is over the limit is dropped here,
// on the agent, and counted, so the drop is visible on a dashboard.
type limiter struct {
	mu        sync.Mutex
	maxSeries int
	series    map[uint64]int64 // series -> unix seconds last seen
	pruned    time.Time

	logRate   float64 // bytes per second
	logTokens float64
	logFilled time.Time

	droppedSeries, droppedLogs, droppedSpool atomic.Uint64
	warned                                   atomic.Int64 // unix seconds of the last warning
}

func (l *limiter) configure(c config.Limits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxSeries, l.logRate = c.MaxSeries, float64(c.MaxLogBytesPerSecond)
	if l.series == nil {
		l.series = map[uint64]int64{}
	}
	l.logTokens, l.logFilled = l.logRate*60, time.Now()
}

// seriesID identifies a series without sorting its labels: the hashes of
// the name and of each label are combined in a way that ignores order.
func seriesID(s *model.Sample) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s.Name))
	id := h.Sum64()
	for k, v := range s.Labels {
		h.Reset()
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(v))
		id ^= h.Sum64() * 1099511628211
	}
	return id
}

// samples returns the batch without samples of series beyond the cap. A
// series counts while it keeps reporting, and for ten minutes after.
func (l *limiter) samples(batch []model.Sample) []model.Sample {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.pruned) > time.Minute {
		l.pruned = now
		for id, seen := range l.series {
			if now.Unix()-seen > 600 {
				delete(l.series, id)
			}
		}
	}
	kept := batch[:0]
	dropped := uint64(0)
	for i := range batch {
		id := seriesID(&batch[i])
		// Zero means no cap (a config built in code, without the defaults).
		if _, known := l.series[id]; !known && l.maxSeries > 0 && len(l.series) >= l.maxSeries {
			dropped++
			continue
		}
		l.series[id] = now.Unix()
		kept = append(kept, batch[i])
	}
	l.droppedSeries.Add(dropped)
	return kept
}

// logs returns the entries that fit the byte rate. A burst of up to a
// minute's worth is let through; beyond that, lines are dropped.
// traceBytes takes n bytes of spans out of the allowance the logs use, and
// reports whether there was room.
func (l *limiter) traceBytes(n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logRate <= 0 {
		return true
	}
	now := time.Now()
	l.logTokens = min(l.logRate*60, l.logTokens+now.Sub(l.logFilled).Seconds()*l.logRate)
	l.logFilled = now
	if float64(n) > l.logTokens {
		l.droppedLogs.Add(uint64(n))
		return false
	}
	l.logTokens -= float64(n)
	return true
}

func (l *limiter) logs(batch []model.LogEntry) []model.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logRate <= 0 {
		return batch
	}
	now := time.Now()
	l.logTokens = min(l.logRate*60, l.logTokens+now.Sub(l.logFilled).Seconds()*l.logRate)
	l.logFilled = now
	kept := batch[:0]
	dropped := uint64(0)
	for i := range batch {
		size := float64(len(batch[i].Msg) + 64)
		for k, v := range batch[i].Fields {
			size += float64(len(k) + len(v))
		}
		if size > l.logTokens {
			dropped += uint64(size)
			continue
		}
		l.logTokens -= size
		kept = append(kept, batch[i])
	}
	l.droppedLogs.Add(dropped)
	return kept
}

// selfMetrics reports the agent's own state: what is waiting to be sent,
// what was dropped and why, and how many series it is tracking.
func (a *Agent) selfMetrics(context.Context) ([]model.Sample, error) {
	ts := time.Now().UnixMilli()
	var out []model.Sample
	add := func(name string, v float64, kv ...string) {
		l := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}
	for _, s := range []*shipper{a.metrics, a.logs, a.events, a.captures, a.traces} {
		add("vigil_agent_spool_bytes", float64(s.spool.Size()), "signal", s.name)
	}
	l := &a.limits
	add("vigil_agent_dropped_total", float64(l.droppedSeries.Load()), "reason", "series_limit", "unit", "samples")
	add("vigil_agent_dropped_total", float64(l.droppedLogs.Load()), "reason", "log_rate_limit", "unit", "bytes")
	add("vigil_agent_dropped_total", float64(l.droppedSpool.Load()), "reason", "spool_full", "unit", "batches")
	l.mu.Lock()
	series := len(l.series)
	l.mu.Unlock()
	add("vigil_agent_series", float64(series))
	a.mu.Lock()
	hash := a.remoteHash
	a.mu.Unlock()
	if hash == "" {
		hash = "local"
	}
	add("vigil_agent_info", 1, "version", a.version, "config", hash)

	// What the agent itself costs the host.
	if v, ok := cpuSeconds(); ok {
		add("vigil_agent_cpu_seconds_total", v)
	}
	if v, ok := residentBytes(); ok {
		add("vigil_agent_memory_bytes", v)
	} else {
		// No resident size to be had here: what the Go runtime got from the system.
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		add("vigil_agent_memory_bytes", float64(m.Sys))
	}
	if v, ok := openFiles(); ok {
		add("vigil_agent_open_files", v)
	}
	add("vigil_agent_goroutines", float64(runtime.NumGoroutine()))
	add("vigil_agent_start_time_seconds", float64(a.started.Unix()))

	// Say it in the log too, but not on every round.
	if dropped := l.droppedSeries.Load() + l.droppedLogs.Load(); dropped > 0 {
		if last := l.warned.Load(); time.Now().Unix()-last > 300 && l.warned.CompareAndSwap(last, time.Now().Unix()) {
			a.log.Warn("limits are dropping data on this host; see vigil_agent_dropped_total",
				"samples_over_series_limit", l.droppedSeries.Load(), "log_bytes_over_rate", l.droppedLogs.Load())
		}
	}
	return out, nil
}
