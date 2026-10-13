package rules

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/model"
)

type fakeBackend struct {
	mu       sync.Mutex
	result   map[string][]Series // by expr
	queryErr error
	notified []alerting.Alert
	recorded []model.Sample
	states   map[string][]byte
}

func newBackend() *fakeBackend {
	return &fakeBackend{result: map[string][]Series{}, states: map[string][]byte{}}
}

func (b *fakeBackend) set(expr string, s ...Series) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.result[expr] = s
}

func (b *fakeBackend) Query(_ context.Context, _, expr string, _ time.Time) ([]Series, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if expr == "bad(" {
		return nil, &QueryError{Bad: true, Msg: "parse error"}
	}
	return b.result[expr], b.queryErr
}

func (b *fakeBackend) Notify(_ context.Context, a []alerting.Alert) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notified = append(b.notified, a...)
	return nil
}

func (b *fakeBackend) Record(_ context.Context, s []model.Sample) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorded = append(b.recorded, s...)
	return nil
}

func (b *fakeBackend) LoadAlertStates() (map[string][]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range b.states {
		out[k] = v
	}
	return out, nil
}

func (b *fakeBackend) SaveAlertState(k string, d []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[k] = d
	return nil
}

func (b *fakeBackend) PruneAlertStates(keep map[string]bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.states {
		if !keep[k] {
			delete(b.states, k)
		}
	}
	return nil
}

// take returns and clears what was sent to the notifier.
func (b *fakeBackend) take() []alerting.Alert {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notified
	b.notified = nil
	return out
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const ruleFile = `
groups:
  - name: host
    interval: 30s
    labels: {team: ops}
    rules:
      - alert: DiskFull
        expr: disk_full
        for: 2m
        keep_firing_for: 1m
        labels:
          severity: '{{ if gt $value 95.0 }}page{{ else }}warn{{ end }}'
        annotations:
          summary: '{{ $labels.mountpoint }} on {{ $labels.instance }} is {{ $value | humanize }}% full'
      - record: instance:disk:max
        expr: disk_full
        labels: {source: rule}
`

// step evaluates the first rule of the manager at a chosen time.
func step(m *Manager, rs *ruleState, at time.Time) {
	m.eval(context.Background(), rs, at)
}

func TestAlertLifecycle(t *testing.T) {
	f, err := Parse(ruleFile)
	if err != nil {
		t.Fatal(err)
	}
	be := newBackend()
	m := NewManager(be, "http://vigil.example", quiet)
	g := &f.Groups[0]
	rs := &ruleState{group: g, rule: &g.Rules[0], key: g.key(&g.Rules[0]), alerts: map[string]*Instance{}}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	series := Series{Labels: map[string]string{"__name__": "disk", "instance": "db-01", "mountpoint": "/data"}, Value: 97.25}

	// Appears: pending, nothing sent yet.
	be.set("disk_full", series)
	step(m, rs, t0)
	if len(rs.alerts) != 1 || len(be.take()) != 0 {
		t.Fatalf("after first eval: %d alerts", len(rs.alerts))
	}
	var in *Instance
	for _, in = range rs.alerts {
	}
	if in.State != StatePending || in.Labels["alertname"] != "DiskFull" || in.Labels["team"] != "ops" ||
		in.Labels["severity"] != "page" || in.Labels["__name__"] != "" {
		t.Fatalf("pending instance: %+v", in)
	}
	if in.Annotations["summary"] != "/data on db-01 is 97.25% full" {
		t.Errorf("summary = %q", in.Annotations["summary"])
	}

	// Still pending before `for` has passed, firing after.
	step(m, rs, t0.Add(time.Minute))
	if in.State != StatePending {
		t.Fatalf("state after 1m: %s", in.State)
	}
	step(m, rs, t0.Add(2*time.Minute))
	sent := be.take()
	if in.State != StateFiring || len(sent) != 1 || !sent[0].EndsAt.After(t0.Add(2*time.Minute)) ||
		!sent[0].StartsAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("firing: state=%s sent=%+v", in.State, sent)
	}

	// A failing query must not resolve the alert; it is re-sent as firing.
	be.queryErr = &QueryError{Msg: "store unreachable"}
	step(m, rs, t0.Add(3*time.Minute))
	sent = be.take()
	if in.State != StateFiring || len(sent) != 1 || rs.health != "err" {
		t.Fatalf("during outage: state=%s sent=%d health=%s", in.State, len(sent), rs.health)
	}
	be.queryErr = nil

	// Gone: keep_firing_for holds it for a minute, then it resolves.
	be.set("disk_full")
	step(m, rs, t0.Add(4*time.Minute))
	if in.State != StateFiring {
		t.Fatalf("keep_firing_for ignored: %s", in.State)
	}
	be.take()
	step(m, rs, t0.Add(5*time.Minute))
	sent = be.take()
	if len(sent) != 1 || !sent[0].EndsAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("resolution: %+v", sent)
	}
	// Coming back while the resolution is still being re-sent starts over as pending.
	be.set("disk_full", series)
	step(m, rs, t0.Add(6*time.Minute))
	if len(rs.alerts) != 1 {
		t.Fatalf("%d alerts", len(rs.alerts))
	}
	for _, in = range rs.alerts {
	}
	if in.State != StatePending || !in.ActiveAt.Equal(t0.Add(6*time.Minute)) {
		t.Fatalf("after return: %+v", in)
	}

	// Two series collapsing into the same labels is an error, not a silent merge.
	rs2 := &ruleState{group: g, rule: &Rule{Alert: "X", Expr: "dup"}, alerts: map[string]*Instance{}}
	be.set("dup", Series{Labels: map[string]string{"__name__": "a"}, Value: 1}, Series{Labels: map[string]string{"__name__": "b"}, Value: 2})
	step(m, rs2, t0)
	if rs2.health != "err" || !strings.Contains(rs2.lastErr, "same labels") {
		t.Errorf("duplicate labels: health=%s err=%q", rs2.health, rs2.lastErr)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestManagerRunsRecordsAndRestores(t *testing.T) {
	text := strings.NewReplacer("interval: 30s", "interval: 20ms", "for: 2m", "for: 0s", "keep_firing_for: 1m", "keep_firing_for: 0s").Replace(ruleFile)
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	be := newBackend()
	be.set("disk_full", Series{Labels: map[string]string{"__name__": "disk", "instance": "db-01"}, Value: 50})
	m := NewManager(be, "", quiet)
	if err := m.Check(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	firing := func(mgr *Manager) *AlertStatus {
		for _, g := range mgr.Status() {
			for _, r := range g.Rules {
				for _, a := range r.Alerts {
					if a.State == StateFiring {
						return &a
					}
				}
			}
		}
		return nil
	}
	waitFor(t, "firing alert", func() bool { return firing(m) != nil })
	first := firing(m)
	if first.Labels["severity"] != "warn" {
		t.Errorf("labels: %v", first.Labels)
	}
	waitFor(t, "recorded samples", func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		var rec, alerts bool
		for _, s := range be.recorded {
			rec = rec || (s.Name == "instance:disk:max" && s.Labels["source"] == "rule" && s.Labels["team"] == "ops" && s.Value == 50)
			alerts = alerts || (s.Name == "ALERTS" && s.Labels["alertstate"] == "firing" && s.Labels["alertname"] == "DiskFull")
		}
		return rec && alerts
	})

	// Re-applying the same file keeps the alert; a restart restores it.
	if err := m.Apply(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if a := firing(m); a == nil || !a.ActiveAt.Equal(first.ActiveAt) {
		t.Fatalf("alert lost on reload: %+v", a)
	}
	m.Stop()
	m2 := NewManager(be, "", quiet)
	if err := m2.Apply(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	defer m2.Stop()
	if a := firing(m2); a == nil || !a.ActiveAt.Equal(first.ActiveAt) {
		t.Fatalf("alert not restored after restart: %+v", a)
	}

	// Removing the rule forgets its saved state.
	empty, _ := Parse("groups: []")
	if err := m2.Apply(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	if states, _ := be.LoadAlertStates(); len(states) != 0 {
		t.Errorf("state left behind: %v", states)
	}

	bad, _ := Parse("groups: [{name: g, rules: [{alert: A, expr: 'bad('}]}]")
	if err := m2.Check(context.Background(), bad); err == nil || !strings.Contains(err.Error(), `rule "A"`) {
		t.Errorf("Check on bad expr: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	for want, text := range map[string]string{
		"defined twice":       "groups: [{name: a}, {name: a}]",
		"name is required":    "groups: [{rules: []}]",
		"exactly one":         "groups: [{name: a, rules: [{expr: up}]}]",
		"expr is required":    "groups: [{name: a, rules: [{alert: A}]}]",
		"type must be":        "groups: [{name: a, type: traces}]",
		"not found":           "groups: [{name: a, rules: [{alert: A, expr: up, fro: 5m}]}]",
		"template in":         "groups: [{name: a, rules: [{alert: A, expr: up, annotations: {s: '{{ nope }}'}}]}]",
		"valid metric name":   "groups: [{name: a, rules: [{record: 'a b', expr: up}]}]",
		"only apply to alert": "groups: [{name: a, rules: [{record: a, expr: up, for: 1m}]}]",
		"label name":          "groups: [{name: a, rules: [{alert: A, expr: up, labels: {alertname: x}}]}]",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want error containing %q", text, err, want)
		}
	}
	if f, err := Parse(""); err != nil || len(f.Groups) != 0 {
		t.Errorf("empty file: %v", err)
	}
	if f, err := Parse("groups: [{name: a, interval: 1d, rules: [{alert: A, expr: up, for: 1h30m}]}]"); err != nil ||
		f.Groups[0].interval() != 24*time.Hour || time.Duration(f.Groups[0].Rules[0].For) != 90*time.Minute {
		t.Errorf("prometheus durations: %v", err)
	}
}

// Editing a rule's expression keeps its alerts; removing the rule resolves
// them at once instead of leaving them to time out.
func TestEditedAndRemovedRules(t *testing.T) {
	text := strings.NewReplacer("interval: 30s", "interval: 20ms", "for: 2m", "for: 0s", "keep_firing_for: 1m", "keep_firing_for: 0s").Replace(ruleFile)
	series := Series{Labels: map[string]string{"__name__": "disk", "instance": "db-01"}, Value: 50}
	be := newBackend()
	be.set("disk_full", series)
	be.set("disk_full > 40", series)
	m := NewManager(be, "", quiet)
	defer m.Stop()
	apply := func(text string) {
		t.Helper()
		f, err := Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Apply(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	firedAt := func() (at time.Time) {
		for _, g := range m.Status() {
			for _, r := range g.Rules {
				for _, a := range r.Alerts {
					if a.State == StateFiring {
						at = a.ActiveAt
					}
				}
			}
		}
		return at
	}
	apply(text)
	waitFor(t, "the alert to fire", func() bool { return !firedAt().IsZero() })
	since := firedAt()

	apply(strings.Replace(text, "expr: disk_full\n        for", "expr: disk_full > 40\n        for", 1))
	time.Sleep(60 * time.Millisecond)
	if got := firedAt(); !got.Equal(since) {
		t.Errorf("after editing the expression the alert is active since %v, was %v", got, since)
	}
	for _, a := range be.take() {
		if !a.EndsAt.After(time.Now()) {
			t.Errorf("editing the expression resolved the alert: %+v", a)
		}
	}

	apply("groups: []\n")
	var ended int
	for _, a := range be.take() {
		if a.Labels["alertname"] == "DiskFull" && !a.EndsAt.After(time.Now()) {
			ended++
		}
	}
	if ended != 1 || !firedAt().IsZero() {
		t.Errorf("removing the rule sent %d resolved alerts, want 1", ended)
	}
}
