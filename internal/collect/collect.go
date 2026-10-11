// Package collect runs the scrape loop shared by the agent and by the
// server's agentless mode: discover targets, scrape them, add host metrics,
// and hand each round to Emit as one batch.
package collect

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/discovery"
	"github.com/ijcodelabs/vigil/internal/hostmetrics"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/scrape"
)

const maxParallel = 16

// Source is something the collector reads on its own interval.
type Source struct {
	Name     string
	Interval time.Duration // zero: the collector's interval
	Collect  func(context.Context) ([]model.Sample, error)
}

type Collector struct {
	Interval    time.Duration
	Discoverers []discovery.Discoverer
	HostMetrics bool
	// Sources are further things to read on a schedule: containers,
	// probes, process groups, systemd.
	Sources []Source
	Labels  map[string]string // attached to every sample
	Emit    func([]model.Sample)
	Log     *slog.Logger

	client *scrape.Client
}

// Run blocks until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	c.client = scrape.NewClient(time.Minute)
	// Jobs may have their own intervals, so the loop wakes up often and
	// each time collects only what is due.
	tick := min(c.Interval, time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	next := map[string]time.Time{} // when each target is due again
	var hostDue time.Time
	for {
		now := time.Now()
		c.round(ctx, now, next, &hostDue)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// round scrapes the targets that are due and, on the agent-wide interval,
// reads host metrics and the extra sources.
func (c *Collector) round(ctx context.Context, now time.Time, next map[string]time.Time, hostDue *time.Time) {
	var due []discovery.Target
	seen := map[string]bool{}
	for _, d := range c.Discoverers {
		ts, err := d.Targets(ctx)
		if err != nil {
			c.Log.Warn("discovery failed", "discoverer", d.Name(), "err", err)
		}
		for _, t := range ts {
			key := t.Job + "\x00" + t.URL
			seen[key] = true
			if now.Before(next[key]) {
				continue
			}
			if t.Interval == 0 {
				t.Interval = c.Interval
			}
			// Step from the previous due time, not from now, so a slow
			// round does not stretch the interval; skip ahead if far behind.
			at := next[key].Add(t.Interval)
			if at.Before(now) {
				at = now.Add(t.Interval)
			}
			next[key] = at
			due = append(due, t)
		}
	}
	var (
		mu    sync.Mutex
		batch []model.Sample
		wg    sync.WaitGroup
		sem   = make(chan struct{}, maxParallel)
	)
	for _, t := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := c.scrapeTarget(ctx, t)
			mu.Lock()
			batch = append(batch, s...)
			mu.Unlock()
		}()
	}
	for _, src := range c.Sources {
		key := "source\x00" + src.Name
		seen[key] = true
		if now.Before(next[key]) {
			continue
		}
		interval := src.Interval
		if interval == 0 {
			interval = c.Interval
		}
		at := next[key].Add(interval)
		if at.Before(now) {
			at = now.Add(interval)
		}
		next[key] = at
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := src.Collect(ctx)
			if err != nil && ctx.Err() == nil {
				c.Log.Warn("metric source incomplete", "source", src.Name, "err", err)
			}
			mu.Lock()
			batch = append(batch, s...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if !now.Before(*hostDue) {
		*hostDue = now.Add(c.Interval)
		if c.HostMetrics {
			hs, err := hostmetrics.Collect(ctx)
			if err != nil && ctx.Err() == nil {
				c.Log.Warn("host metrics incomplete", "err", err)
			}
			batch = append(batch, hs...)
		}
	}
	for key := range next {
		if !seen[key] {
			delete(next, key)
		}
	}
	if ctx.Err() != nil || len(batch) == 0 {
		return
	}
	for i := range batch {
		for k, v := range c.Labels {
			setLabel(batch[i].Labels, k, v)
		}
	}
	c.Emit(batch)
}

func matchesAny(res []*regexp.Regexp, name string) bool {
	for _, re := range res {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// scrapeTarget returns the target's samples plus the synthetic `up` and
// `scrape_duration_seconds` series, as Prometheus does.
func (c *Collector) scrapeTarget(ctx context.Context, t discovery.Target) []model.Sample {
	timeout := t.Timeout
	if timeout == 0 {
		timeout = min(t.Interval, 10*time.Second)
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	samples, err := c.client.Scrape(sctx, scrape.Request{URL: t.URL, Insecure: t.Insecure,
		BearerToken: t.BearerToken, Username: t.Username, Password: t.Password})
	up := 1.0
	if err != nil {
		up, samples = 0, nil
		if ctx.Err() == nil {
			c.Log.Warn("scrape failed", "job", t.Job, "instance", t.Instance, "err", err)
		}
	}
	if len(t.Keep) > 0 || len(t.Drop) > 0 {
		kept := samples[:0]
		for _, s := range samples {
			if (len(t.Keep) == 0 || matchesAny(t.Keep, s.Name)) && !matchesAny(t.Drop, s.Name) {
				kept = append(kept, s)
			}
		}
		samples = kept
	}
	ts := start.UnixMilli()
	samples = append(samples,
		model.Sample{Name: "up", Labels: map[string]string{}, Value: up, TS: ts},
		model.Sample{Name: "scrape_duration_seconds", Labels: map[string]string{}, Value: time.Since(start).Seconds(), TS: ts},
	)
	for i := range samples {
		setLabel(samples[i].Labels, "job", t.Job)
		setLabel(samples[i].Labels, "instance", t.Instance)
		for k, v := range t.Labels {
			setLabel(samples[i].Labels, k, v)
		}
	}
	return samples
}

// setLabel sets k=v; a clashing label exposed by the target itself is kept
// as exported_<k>, matching Prometheus' default.
func setLabel(l map[string]string, k, v string) {
	if old, ok := l[k]; ok && old != v {
		l["exported_"+k] = old
	}
	l[k] = v
}
