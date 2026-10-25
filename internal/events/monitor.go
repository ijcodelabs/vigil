package events

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

// ErrUnsupported means runtime events cannot run on this host. The agent
// carries on without them.
var ErrUnsupported = errors.New("runtime events unavailable")

// source is where raw kernel records come from.
type source interface {
	Read() ([]byte, error) // blocks; fails once closed
	BootTime() time.Time
	Lost() uint64
	Close() error
}

// Monitor runs the probes, the rules and the flight recorder for one agent.
type Monitor struct {
	cfg *config.Security
	log *slog.Logger

	// ContainerNames resolves container ids to names; optional.
	ContainerNames func() map[string]string
	// SendDetections and SendCapture hand results to the agent's shippers.
	SendDetections func([]Detection) error
	SendCapture    func(*Capture) error

	seen       [7]atomic.Uint64 // events by type index
	detections atomic.Uint64
	suppressed atomic.Uint64
	lost       atomic.Uint64
	running    atomic.Bool
}

func NewMonitor(cfg *config.Security, log *slog.Logger) *Monitor {
	return &Monitor{cfg: cfg, log: log}
}

var typeIndex = map[string]int{TypeExec: 1, TypeExit: 2, TypeConnect: 3, TypeListen: 4, TypeOpen: 5, TypeSetuid: 6}

// Run blocks until ctx is cancelled. It returns ErrUnsupported (wrapped
// with the reason) when the kernel cannot run the probes.
func (m *Monitor) Run(ctx context.Context) error {
	engine, err := NewEngine(m.cfg)
	if err != nil {
		return err
	}
	src, err := openSource(m.cfg, m.log)
	if err != nil {
		return err
	}
	return m.run(ctx, engine, src)
}

func (m *Monitor) run(ctx context.Context, engine *Engine, src source) error {
	m.running.Store(true)
	defer m.running.Store(false)
	m.log.Info("runtime events on", "rules", len(m.cfg.Rules), "watch_paths", len(m.cfg.WatchPaths))

	raw := make(chan []byte, 8192)
	go func() {
		defer close(raw)
		for {
			b, err := src.Read()
			if err != nil {
				return
			}
			select {
			case raw <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer src.Close()

	var batch []Detection
	engine.OnDetection = func(d Detection) {
		m.detections.Add(1)
		batch = append(batch, d)
	}
	engine.OnCapture = func(c *Capture) {
		if err := m.SendCapture(c); err != nil {
			m.log.Error("capture lost", "rule", c.Meta.Rule, "err", err)
		}
	}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := m.SendDetections(batch); err != nil {
			m.log.Error("detections lost", "count", len(batch), "err", err)
		}
		batch = nil
	}

	procs, conts := newProcCache(), newContainers(m.ContainerNames)
	boot := src.BootTime()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case b, ok := <-raw:
			if !ok {
				flush()
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("events: kernel event stream ended")
			}
			ev, err := decode(b, boot)
			if err != nil {
				continue
			}
			m.seen[typeIndex[ev.Type]].Add(1)
			procs.apply(ev)
			conts.apply(ev)
			engine.Handle(ev)
			if len(batch) >= 200 {
				flush()
			}
		case now := <-tick.C:
			engine.Tick(now)
			flush()
			m.suppressed.Store(engine.Suppressed)
			m.lost.Store(src.Lost())
		}
	}
}

// Metrics reports the monitor's own counters with the agent's metrics.
func (m *Monitor) Metrics(context.Context) ([]model.Sample, error) {
	if !m.running.Load() {
		return nil, nil
	}
	ts := time.Now().UnixMilli()
	out := []model.Sample{
		{Name: "vigil_events_detections_total", Labels: map[string]string{}, Value: float64(m.detections.Load()), TS: ts},
		{Name: "vigil_events_suppressed_total", Labels: map[string]string{}, Value: float64(m.suppressed.Load()), TS: ts},
		{Name: "vigil_events_lost_total", Labels: map[string]string{}, Value: float64(m.lost.Load()), TS: ts},
	}
	for name, i := range typeIndex {
		out = append(out, model.Sample{Name: "vigil_events_seen_total", Labels: map[string]string{"type": name},
			Value: float64(m.seen[i].Load()), TS: ts})
	}
	return out, nil
}

// EncodeDetections is the agent→server wire format for detections:
// gzip-compressed JSON lines.
func EncodeDetections(ds []Detection) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for i := range ds {
		enc.Encode(&ds[i])
	}
	zw.Close()
	return buf.Bytes()
}

// Dump prints every event (and every detection) as a JSON line until ctx is
// cancelled. It is what `vigil-agent events` runs: a way to see what the probes
// report on a host and to try rules out.
func Dump(ctx context.Context, cfg *config.Security, w io.Writer, log *slog.Logger) error {
	engine, err := NewEngine(cfg)
	if err != nil {
		return err
	}
	src, err := openSource(cfg, log)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); src.Close() }()
	enc := json.NewEncoder(w)
	engine.OnDetection = func(d Detection) {
		enc.Encode(map[string]any{"detection": d.Rule, "severity": d.Severity, "summary": d.Summary})
	}
	procs, conts := newProcCache(), newContainers(nil)
	for {
		b, err := src.Read()
		if err != nil {
			return nil
		}
		ev, err := decode(b, src.BootTime())
		if err != nil {
			continue
		}
		procs.apply(ev)
		conts.apply(ev)
		enc.Encode(ev)
		engine.Handle(ev)
	}
}
