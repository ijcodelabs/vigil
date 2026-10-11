package events

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

const (
	maxRecorded     = 200000 // events kept in memory, whatever the time window
	maxCaptureEvent = 50000
)

// Detection is a rule that matched an event.
type Detection struct {
	Time      time.Time `json:"time"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Summary   string    `json:"summary"`
	CaptureID string    `json:"capture_id,omitempty"`
	Event     *Event    `json:"event"`
}

// CaptureMeta describes a capture: the activity around a detection.
type CaptureMeta struct {
	ID        string    `json:"id"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Summary   string    `json:"summary"`
	Time      time.Time `json:"time"` // of the triggering event
	From      time.Time `json:"from"`
	Until     time.Time `json:"until"`
	Events    int       `json:"events"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Capture is what is stored and shown: the events between From and Until.
type Capture struct {
	Meta    CaptureMeta `json:"meta"`
	Trigger *Event      `json:"trigger"`
	Events  []*Event    `json:"events"`
}

// Encode returns the capture as gzip-compressed JSON.
func (c *Capture) Encode() []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	json.NewEncoder(zw).Encode(c)
	zw.Close()
	return buf.Bytes()
}

// recorder is the flight recorder: the events of the last `window`.
type recorder struct {
	window time.Duration
	evs    []*Event
}

func (r *recorder) add(ev *Event) {
	r.evs = append(r.evs, ev)
	cut := 0
	limit := ev.Time.Add(-r.window)
	for cut < len(r.evs) && (r.evs[cut].Time.Before(limit) || len(r.evs)-cut > maxRecorded) {
		cut++
	}
	// Drop the old part only once it is worth copying the rest down.
	if cut > 1024 && cut > len(r.evs)/2 {
		r.evs = append(r.evs[:0:0], r.evs[cut:]...)
	}
}

func (r *recorder) between(from, until time.Time) []*Event {
	var out []*Event
	for _, ev := range r.evs {
		if !ev.Time.Before(from) && !ev.Time.After(until) {
			out = append(out, ev)
		}
	}
	return out
}

type ruleState struct {
	windowStart time.Time
	count       int
	lastCapture time.Time
}

// Engine evaluates rules on events in order and assembles captures. It is
// not safe for concurrent use; one goroutine feeds it.
type Engine struct {
	cfg   *config.Security
	rules []*rule
	state map[string]*ruleState
	rec   recorder

	pending *Capture // a capture still collecting its "after" part

	// OnDetection and OnCapture receive the results.
	OnDetection func(Detection)
	OnCapture   func(*Capture)

	Suppressed uint64 // detections not reported because of max_per_minute
}

func NewEngine(cfg *config.Security) (*Engine, error) {
	rules, err := compileRules(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg, rules: rules, state: map[string]*ruleState{},
		rec: recorder{window: cfg.Buffer.Std()}}, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Handle records an event and runs the rules on it.
func (e *Engine) Handle(ev *Event) {
	e.Tick(ev.Time)
	e.rec.add(ev)
	if len(e.rules) == 0 {
		return
	}
	vars := ev.vars()
	for _, r := range e.rules {
		if !r.matches(vars) {
			continue
		}
		st := e.state[r.Name]
		if st == nil {
			st = &ruleState{}
			e.state[r.Name] = st
		}
		if ev.Time.Sub(st.windowStart) >= time.Minute {
			st.windowStart, st.count = ev.Time, 0
		}
		if st.count++; st.count > r.MaxPerMinute {
			e.Suppressed++
			continue
		}
		d := Detection{Time: ev.Time, Rule: r.Name, Severity: r.Severity, Summary: r.summary(vars), Event: ev}
		// One capture at a time: a detection during a running capture is
		// inside its window anyway and is linked to it.
		if e.pending != nil {
			d.CaptureID = e.pending.Meta.ID
		} else if r.Capture && (st.lastCapture.IsZero() || ev.Time.Sub(st.lastCapture) >= e.cfg.CaptureCooldown.Std()) {
			st.lastCapture = ev.Time
			e.pending = &Capture{Trigger: ev, Meta: CaptureMeta{ID: newID(), Rule: r.Name, Severity: r.Severity,
				Summary: d.Summary, Time: ev.Time, From: ev.Time.Add(-e.cfg.CaptureBefore.Std()),
				Until: ev.Time.Add(e.cfg.CaptureAfter.Std())}}
			d.CaptureID = e.pending.Meta.ID
		}
		if e.OnDetection != nil {
			e.OnDetection(d)
		}
	}
}

// Tick finishes a capture whose "after" time has passed. Handle calls it;
// the caller also calls it on a timer so a quiet host still finishes.
func (e *Engine) Tick(now time.Time) {
	c := e.pending
	if c == nil || !now.After(c.Meta.Until) {
		return
	}
	e.pending = nil
	c.Events = e.rec.between(c.Meta.From, c.Meta.Until)
	if len(c.Events) > maxCaptureEvent {
		// Keep the part closest to the trigger's past: drop from the end.
		c.Events, c.Meta.Truncated = c.Events[:maxCaptureEvent], true
	}
	c.Meta.Events = len(c.Events)
	if e.OnCapture != nil {
		e.OnCapture(c)
	}
}
