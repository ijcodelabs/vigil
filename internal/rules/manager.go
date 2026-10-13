package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/model"
)

const (
	StatePending  = "pending"
	StateFiring   = "firing"
	stateResolved = "resolved"

	// A resolution is re-sent for this long, then the alert is forgotten.
	resolvedRetention = 5 * time.Minute
	minResend         = time.Minute
)

// Series is one row of a query result.
type Series struct {
	Labels map[string]string
	Value  float64
}

// QueryError is a failed query. Bad means the store rejected the expression
// itself, as opposed to being unreachable.
type QueryError struct {
	Bad bool
	Msg string
}

func (e *QueryError) Error() string { return e.Msg }

// Backend is what the manager needs from the rest of the server.
type Backend interface {
	// Query runs an instant query against the metric or the log store.
	Query(ctx context.Context, typ, expr string, at time.Time) ([]Series, error)
	// Notify delivers firing and resolved alerts.
	Notify(ctx context.Context, alerts []alerting.Alert) error
	// Record stores samples produced by rules.
	Record(ctx context.Context, samples []model.Sample) error
	// Alert state survives restarts through these three.
	LoadAlertStates() (map[string][]byte, error)
	SaveAlertState(key string, data []byte) error
	PruneAlertStates(keep map[string]bool) error
}

// Instance is one alert: a rule plus one label set it matched.
type Instance struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	Value       float64           `json:"value"`
	ActiveAt    time.Time         `json:"activeAt"`
	FiredAt     time.Time         `json:"firedAt,omitzero"`
	ResolvedAt  time.Time         `json:"resolvedAt,omitzero"`
	// MissingSince is when a firing alert's series first stopped showing up
	// in a successful evaluation; keep_firing_for counts from here.
	MissingSince time.Time `json:"missingSince,omitzero"`
}

type ruleState struct {
	group *Group
	rule  *Rule
	key   string

	mu       sync.Mutex
	alerts   map[string]*Instance // by label fingerprint
	health   string
	lastErr  string
	lastEval time.Time
	evalTook time.Duration
}

type Manager struct {
	be          Backend
	externalURL string
	log         *slog.Logger

	mu     sync.Mutex
	rules  []*ruleState
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewManager(be Backend, externalURL string, log *slog.Logger) *Manager {
	return &Manager{be: be, externalURL: externalURL, log: log}
}

// Check runs every expression once, so a rule file with a syntax error is
// refused when it is saved rather than failing quietly later.
func (m *Manager) Check(ctx context.Context, f *File) error {
	for gi := range f.Groups {
		g := &f.Groups[gi]
		for ri := range g.Rules {
			r := &g.Rules[ri]
			if _, err := m.be.Query(ctx, g.Type, r.Expr, time.Now()); err != nil {
				return fmt.Errorf("group %q, rule %q: %w", g.Name, r.name(), err)
			}
		}
	}
	return nil
}

// Apply replaces the running rules. Alerts of rules that are unchanged keep
// their state; on the first call it is restored from the backend.
func (m *Manager) Apply(ctx context.Context, f *File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]map[string]*Instance{}
	if m.cancel == nil {
		saved, err := m.be.LoadAlertStates()
		if err != nil {
			return fmt.Errorf("loading alert state: %w", err)
		}
		for key, data := range saved {
			var list []*Instance
			if json.Unmarshal(data, &list) == nil {
				old[key] = map[string]*Instance{}
				for _, in := range list {
					old[key][fingerprint(in.Labels)] = in
				}
			}
		}
	} else {
		m.cancel()
		m.wg.Wait()
		for _, rs := range m.rules {
			old[rs.key] = rs.alerts
		}
	}

	m.rules = nil
	keep := map[string]bool{}
	for gi := range f.Groups {
		for ri := range f.Groups[gi].Rules {
			keep[f.Groups[gi].key(&f.Groups[gi].Rules[ri])] = true
		}
	}
	// A rule whose expression was edited has a new key, but it is the same
	// rule to whoever edited it: its alerts carry on instead of resolving
	// and starting over. That holds when exactly one rule of that group and
	// name went and exactly one came.
	gone, came := map[string][]string{}, map[string]int{}
	for key := range old {
		if !keep[key] {
			gone[ruleName(key)] = append(gone[ruleName(key)], key)
		}
	}
	for key := range keep {
		if old[key] == nil {
			came[ruleName(key)]++
		}
	}
	parent := ctx
	ctx, m.cancel = context.WithCancel(ctx)
	for gi := range f.Groups {
		g := &f.Groups[gi]
		var group []*ruleState
		for ri := range g.Rules {
			rs := &ruleState{group: g, rule: &g.Rules[ri], key: g.key(&g.Rules[ri]), health: "unknown"}
			name := ruleName(rs.key)
			if rs.alerts = old[rs.key]; rs.alerts == nil && len(gone[name]) == 1 && came[name] == 1 {
				rs.alerts = old[gone[name][0]]
				delete(gone, name)
			}
			if rs.alerts == nil {
				rs.alerts = map[string]*Instance{}
			}
			group = append(group, rs)
		}
		m.rules = append(m.rules, group...)
		m.wg.Go(func() { m.runGroup(ctx, g, group) })
	}
	// What is left belonged to rules that are no longer there. Nobody will
	// evaluate them again, so their firing alerts are resolved now; left
	// alone they would stay with the notifier until they time out.
	var resolved []alerting.Alert
	now := time.Now()
	for _, keys := range gone {
		for _, key := range keys {
			for _, in := range old[key] {
				if in.State == StateFiring {
					resolved = append(resolved, alerting.Alert{Labels: in.Labels, Annotations: in.Annotations,
						StartsAt: in.FiredAt, EndsAt: now, GeneratorURL: m.externalURL})
				}
			}
		}
	}
	if len(resolved) > 0 {
		if err := m.be.Notify(parent, resolved); err != nil {
			m.log.Error("resolving the alerts of removed rules failed", "err", err)
		}
	}
	return m.be.PruneAlertStates(keep)
}

// ruleName is a rule's key without its expression: the group and the name.
func ruleName(key string) string { return key[:strings.LastIndex(key, "\x00")+1] }

// Stop ends evaluation; alert state stays saved for the next start.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.wg.Wait()
	}
}

func (m *Manager) runGroup(ctx context.Context, g *Group, rules []*ruleState) {
	t := time.NewTicker(g.interval())
	defer t.Stop()
	for {
		for _, rs := range rules {
			if ctx.Err() != nil {
				return
			}
			m.eval(ctx, rs, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) eval(ctx context.Context, rs *ruleState, now time.Time) {
	series, err := m.be.Query(ctx, rs.group.Type, rs.rule.Expr, now)
	if ctx.Err() != nil {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.lastEval, rs.evalTook = now, time.Since(now)
	if err == nil {
		if rs.rule.Record != "" {
			err = m.record(ctx, rs, series, now)
		} else {
			err = rs.update(series, now, m.externalURL)
		}
	}
	if err != nil {
		rs.health, rs.lastErr = "err", err.Error()
		m.log.Warn("rule evaluation failed", "group", rs.group.Name, "rule", rs.rule.name(), "err", err)
	} else {
		rs.health, rs.lastErr = "ok", ""
	}
	if rs.rule.Alert == "" {
		return
	}
	// Even after a failed evaluation the known alerts are re-sent, so a
	// storage hiccup does not make them look resolved.
	m.publish(ctx, rs, now)
}

func (m *Manager) record(ctx context.Context, rs *ruleState, series []Series, now time.Time) error {
	samples := make([]model.Sample, 0, len(series))
	for _, s := range series {
		l := mergeLabels(s.Labels, rs.group.Labels, rs.rule.Labels)
		samples = append(samples, model.Sample{Name: rs.rule.Record, Labels: l, Value: s.Value, TS: now.UnixMilli()})
	}
	if len(samples) == 0 {
		return nil
	}
	return m.be.Record(ctx, samples)
}

// update moves the rule's alerts through pending → firing → resolved.
func (rs *ruleState) update(series []Series, now time.Time, externalURL string) error {
	r := rs.rule
	seen := map[string]bool{}
	fresh := map[string]*Instance{}
	for _, s := range series {
		src := mergeLabels(s.Labels)
		d := tmplData{Labels: src, Value: s.Value, ExternalURL: externalURL}
		labels := mergeLabels(s.Labels, rs.group.Labels)
		for k, v := range r.Labels {
			labels[k] = expand(v, d)
		}
		labels["alertname"] = r.Alert
		fp := fingerprint(labels)
		if seen[fp] {
			return fmt.Errorf("two result series end up with the same labels %v; add a label that tells them apart", labels)
		}
		seen[fp] = true
		in := &Instance{Labels: labels, Annotations: map[string]string{}, Value: s.Value}
		for k, v := range r.Annotations {
			in.Annotations[k] = expand(v, d)
		}
		fresh[fp] = in
	}

	for fp, in := range fresh {
		cur := rs.alerts[fp]
		if cur == nil || cur.State == stateResolved {
			in.State, in.ActiveAt = StatePending, now
			rs.alerts[fp] = in
			cur = in
		} else {
			cur.Annotations, cur.Value, cur.MissingSince = in.Annotations, in.Value, time.Time{}
		}
		if cur.State == StatePending && now.Sub(cur.ActiveAt) >= time.Duration(r.For) {
			cur.State, cur.FiredAt = StateFiring, now
		}
	}
	for fp, in := range rs.alerts {
		if seen[fp] {
			continue
		}
		switch in.State {
		case StatePending:
			delete(rs.alerts, fp)
		case StateFiring:
			if in.MissingSince.IsZero() {
				in.MissingSince = now
			}
			if now.Sub(in.MissingSince) >= time.Duration(r.KeepFiringFor) {
				in.State, in.ResolvedAt = stateResolved, now
			}
		case stateResolved:
			if now.Sub(in.ResolvedAt) > resolvedRetention {
				delete(rs.alerts, fp)
			}
		}
	}
	return nil
}

// publish saves the rule's alert state, sends firing and resolved alerts to
// the notifier and writes the ALERTS series. Called with rs.mu held.
func (m *Manager) publish(ctx context.Context, rs *ruleState, now time.Time) {
	var (
		out     []alerting.Alert
		samples []model.Sample
		persist []*Instance
	)
	ttl := 4 * max(rs.group.interval(), minResend)
	for _, in := range rs.alerts {
		switch in.State {
		case StateFiring:
			out = append(out, alerting.Alert{Labels: in.Labels, Annotations: in.Annotations,
				StartsAt: in.FiredAt, EndsAt: now.Add(ttl), GeneratorURL: m.externalURL})
		case stateResolved:
			out = append(out, alerting.Alert{Labels: in.Labels, Annotations: in.Annotations,
				StartsAt: in.FiredAt, EndsAt: in.ResolvedAt, GeneratorURL: m.externalURL})
			continue
		}
		persist = append(persist, in)
		l := mergeLabels(in.Labels)
		l["alertstate"] = in.State
		samples = append(samples, model.Sample{Name: "ALERTS", Labels: l, Value: 1, TS: now.UnixMilli()})
	}
	data, _ := json.Marshal(persist)
	if err := m.be.SaveAlertState(rs.key, data); err != nil {
		m.log.Error("saving alert state failed", "rule", rs.rule.Alert, "err", err)
	}
	if len(out) > 0 {
		if err := m.be.Notify(ctx, out); err != nil {
			m.log.Error("handing alerts to the notifier failed", "rule", rs.rule.Alert, "err", err)
		}
	}
	if len(samples) > 0 {
		if err := m.be.Record(ctx, samples); err != nil && !errors.Is(err, context.Canceled) {
			m.log.Warn("writing ALERTS series failed", "rule", rs.rule.Alert, "err", err)
		}
	}
}

// mergeLabels copies the maps into a new one, later maps winning, without
// the metric name.
func mergeLabels(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			if k != "__name__" {
				out[k] = v
			}
		}
	}
	return out
}

func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0xff)
		b.WriteString(labels[k])
		b.WriteByte(0xff)
	}
	return b.String()
}

// Status types, shaped like Prometheus' /api/v1/rules and /api/v1/alerts.

type AlertStatus struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    time.Time         `json:"activeAt"`
	Value       string            `json:"value"`
}

type RuleStatus struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"` // alerting or recording
	Query          string            `json:"query"`
	Duration       float64           `json:"duration"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
	Health         string            `json:"health"`
	LastError      string            `json:"lastError,omitempty"`
	LastEvaluation time.Time         `json:"lastEvaluation"`
	EvaluationTime float64           `json:"evaluationTime"`
	State          string            `json:"state,omitempty"` // inactive, pending or firing
	Alerts         []AlertStatus     `json:"alerts,omitempty"`
}

type GroupStatus struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Interval float64      `json:"interval"`
	Rules    []RuleStatus `json:"rules"`
}

// Status reports every group with the health and alerts of its rules.
func (m *Manager) Status() []GroupStatus {
	m.mu.Lock()
	rules := m.rules
	m.mu.Unlock()
	var out []GroupStatus
	for _, rs := range rules {
		if len(out) == 0 || out[len(out)-1].Name != rs.group.Name {
			out = append(out, GroupStatus{Name: rs.group.Name, Type: rs.group.Type,
				Interval: rs.group.interval().Seconds(), Rules: []RuleStatus{}})
		}
		rs.mu.Lock()
		st := RuleStatus{Name: rs.rule.name(), Type: "recording", Query: rs.rule.Expr, Labels: rs.rule.Labels,
			Health: rs.health, LastError: rs.lastErr, LastEvaluation: rs.lastEval, EvaluationTime: rs.evalTook.Seconds()}
		if rs.rule.Alert != "" {
			st.Type, st.State = "alerting", "inactive"
			st.Duration, st.Annotations = time.Duration(rs.rule.For).Seconds(), rs.rule.Annotations
			for _, in := range rs.alerts {
				if in.State == stateResolved {
					continue
				}
				if in.State == StateFiring || st.State == "inactive" {
					st.State = in.State
				}
				st.Alerts = append(st.Alerts, AlertStatus{Labels: in.Labels, Annotations: in.Annotations,
					State: in.State, ActiveAt: in.ActiveAt, Value: fmt.Sprint(in.Value)})
			}
			sort.Slice(st.Alerts, func(i, j int) bool {
				return fingerprint(st.Alerts[i].Labels) < fingerprint(st.Alerts[j].Labels)
			})
		}
		rs.mu.Unlock()
		g := &out[len(out)-1]
		g.Rules = append(g.Rules, st)
	}
	return out
}
