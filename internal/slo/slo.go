// Package slo holds service level objectives: what share of events has to
// be good over a window, and how that is measured. An SLO is a YAML
// document; the server compiles each one into a rule group that records,
// once a minute, how many events were good and how many there were.
// Everything else (error ratio, budget left, burn rates, reports) is
// sum_over_time over those two series. See memory/context/slo.md.
package slo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/rules"
)

// The recorded series. Counted per minute rather than as a rate over the
// window: summed minute by minute they stay right when the series under
// them come and go (pods, restarts), which rate() over long windows did
// not for the AI cost rules.
const (
	GoodMetric      = "slo_good_per_minute"
	TotalMetric     = "slo_total_per_minute"
	ObjectiveMetric = "slo_objective"

	// GroupPrefix starts the name of every generated rule group.
	GroupPrefix = "slo:"

	maxText = 64 << 10

	// Delay is how long after a minute ends it is counted: the metric store
	// hides its last 30 seconds, and agents push every 15 seconds or so.
	Delay = rules.AlignDelay
)

// Windows an SLO may be judged over.
var windows = map[string]time.Duration{
	"7d": 7 * 24 * time.Hour, "28d": 28 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
	CalendarMonth: 0,
}

const (
	CalendarMonth = "calendar_month"
	DefaultWindow = "30d"
)

// SpanBuckets are the bounds of traces_span_duration_seconds as the agent
// writes them (internal/otlp/traces.go). A latency bound must be one of
// them: between two bounds the share of slow spans is not known.
var SpanBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type SLO struct {
	Name        string            `yaml:"name" json:"name"`
	Service     string            `yaml:"service,omitempty" json:"service,omitempty"`
	Owner       string            `yaml:"owner,omitempty" json:"owner,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Objective   float64           `yaml:"objective" json:"objective"` // percent
	Window      string            `yaml:"window,omitempty" json:"window"`
	Indicator   Indicator         `yaml:"indicator" json:"indicator"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	RunbookURL  string            `yaml:"runbook_url,omitempty" json:"runbook_url,omitempty"`
}

// Indicator says how events are counted; exactly one kind is set.
type Indicator struct {
	Probe *ProbeIndicator `yaml:"probe,omitempty" json:"probe,omitempty"`
	Span  *SpanIndicator  `yaml:"span,omitempty" json:"span,omitempty"`
	Ratio *RatioIndicator `yaml:"ratio,omitempty" json:"ratio,omitempty"`
}

// ProbeIndicator is time-based: every check of a probe is an event, a
// successful one is good. All agents that run the probe count together.
type ProbeIndicator struct {
	Name   string   `yaml:"name" json:"name"`
	Agents []string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// SpanIndicator is request-based, from the span counters the agent keeps
// for every span before sampling.
type SpanIndicator struct {
	Service  string   `yaml:"service" json:"service"`
	Kind     string   `yaml:"kind,omitempty" json:"kind,omitempty"`
	SpanName []string `yaml:"span_name,omitempty" json:"span_name,omitempty"`
	// LatencyBelow makes a span that took longer bad too.
	LatencyBelow model.Duration `yaml:"latency_below,omitempty" json:"latency_below,omitempty"`
}

// RatioIndicator counts with any counters: total, and either good or bad,
// each a series selector such as
// haproxy_backend_http_responses_total{proxy="shop",code="5xx"}.
type RatioIndicator struct {
	Good  string `yaml:"good,omitempty" json:"good,omitempty"`
	Bad   string `yaml:"bad,omitempty" json:"bad,omitempty"`
	Total string `yaml:"total" json:"total"`
}

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	labelRE    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	selectorRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*\s*(\{[^{}\[\]]*\})?$`)
	spanKinds  = map[string]bool{"server": true, "consumer": true, "client": true, "producer": true, "internal": true}
	// Labels the SLO's own series carry; an SLO's labels may not set them.
	ownLabels = map[string]bool{"slo": true, "service": true, "owner": true, "__name__": true}
)

// Parse reads one SLO document. name is what it is saved as: the
// document's own name, if it has one, must be the same. Unknown fields are
// errors, so a typo cannot quietly change what is measured.
func Parse(name, text string) (*SLO, error) {
	if len(text) > maxText {
		return nil, fmt.Errorf("an SLO document is at most %d KiB", maxText>>10)
	}
	s := &SLO{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the SLO document is empty")
		}
		return nil, err
	}
	switch {
	case s.Name == "":
		s.Name = name
	case name != "" && s.Name != name:
		return nil, fmt.Errorf("the document is SLO %q, but it is being saved as %q", s.Name, name)
	}
	return s, s.Check()
}

// Check fills in defaults and says what is wrong.
func (s *SLO) Check() error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("SLO name %q: lower-case letters, digits and dashes, up to 63, not starting or ending with a dash", s.Name)
	}
	if s.Objective <= 50 || s.Objective >= 100 {
		return fmt.Errorf("objective %g: a percentage above 50 and below 100, e.g. 99.9", s.Objective)
	}
	if s.Window == "" {
		s.Window = DefaultWindow
	}
	if _, ok := windows[s.Window]; !ok {
		return fmt.Errorf("window %q: one of 7d, 28d, 30d or %s", s.Window, CalendarMonth)
	}
	for k := range s.Labels {
		if !labelRE.MatchString(k) || strings.HasPrefix(k, "__") {
			return fmt.Errorf("label %q is not a valid label name", k)
		}
		if ownLabels[k] {
			return fmt.Errorf("label %q is set by the SLO itself (slo, service and owner); use the field instead", k)
		}
	}
	if strings.ContainsAny(s.Service+s.Owner, "\r\n") {
		return errors.New("service and owner are one line each")
	}
	ind := &s.Indicator
	n := 0
	for _, set := range []bool{ind.Probe != nil, ind.Span != nil, ind.Ratio != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("indicator: set exactly one of probe, span and ratio")
	}
	switch {
	case ind.Probe != nil:
		if ind.Probe.Name == "" {
			return errors.New("indicator.probe: name is required (the probe's name in the agent config)")
		}
	case ind.Span != nil:
		sp := ind.Span
		if sp.Service == "" {
			return errors.New("indicator.span: service is required")
		}
		if sp.Kind == "" {
			sp.Kind = "server"
		}
		if !spanKinds[sp.Kind] {
			return fmt.Errorf("indicator.span: kind %q is not one of server, consumer, client, producer, internal", sp.Kind)
		}
		if sp.LatencyBelow != 0 {
			if _, err := bucketFor(time.Duration(sp.LatencyBelow)); err != nil {
				return fmt.Errorf("indicator.span: %w", err)
			}
		}
		if s.Service == "" {
			s.Service = sp.Service
		}
	case ind.Ratio != nil:
		r := ind.Ratio
		if (r.Good == "") == (r.Bad == "") {
			return errors.New("indicator.ratio: set exactly one of good and bad, next to total")
		}
		for field, sel := range map[string]string{"good": r.Good, "bad": r.Bad, "total": r.Total} {
			if field == "total" && sel == "" {
				return errors.New("indicator.ratio: total is required")
			}
			if sel != "" && !selectorRE.MatchString(strings.TrimSpace(sel)) {
				return fmt.Errorf("indicator.ratio.%s: %q is not a series selector (a counter's name with optional {label matchers}); vigil counts its increase per minute itself", field, sel)
			}
		}
	}
	return nil
}

// bucketFor finds the span bucket bound equal to d.
func bucketFor(d time.Duration) (string, error) {
	sec := d.Seconds()
	for i, b := range SpanBuckets {
		if b == sec {
			return strconv.FormatFloat(b, 'g', -1, 64), nil
		}
		if b > sec {
			lower := "none"
			if i > 0 {
				lower = fmtSeconds(SpanBuckets[i-1])
			}
			return "", fmt.Errorf("latency_below %s is not a bucket bound of the span histogram; the nearest are %s and %s", d, lower, fmtSeconds(b))
		}
	}
	return "", fmt.Errorf("latency_below %s is above the largest bucket bound of the span histogram (30s)", d)
}

func fmtSeconds(sec float64) string {
	return time.Duration(sec * float64(time.Second)).String()
}

// WindowDuration is the window's length; a calendar month counts as 30
// days where a length is needed (burn-rate factors).
func (s *SLO) WindowDuration() time.Duration {
	if d := windows[s.Window]; d > 0 {
		return d
	}
	return 30 * 24 * time.Hour
}

// Ratio is the objective as a share of 1, without the float noise of
// dividing by 100 (99.9/100 is 0.9990000000000001).
func (s *SLO) Ratio() float64 { return math.Round(s.Objective*1e8) / 1e10 }

// Kind names the indicator's kind.
func (s *SLO) Kind() string {
	switch {
	case s.Indicator.Probe != nil:
		return "probe"
	case s.Indicator.Span != nil:
		return "span"
	default:
		return "ratio"
	}
}

// Queries are the two per-minute counts, as PromQL. Good is given only
// when total is: a minute without events has neither, which keeps the
// coverage (minutes recorded) the same for both.
func (s *SLO) Queries() (good, total string) {
	ind := s.Indicator
	switch {
	case ind.Probe != nil:
		sel := selector("probe_success", "probe", ind.Probe.Name, "agent", ind.Probe.Agents)
		total = fmt.Sprintf("sum(count_over_time(%s[1m]))", sel)
		good = fmt.Sprintf("sum(sum_over_time(%s[1m]))", sel)
	case ind.Span != nil:
		sp := ind.Span
		matchers := []string{"service=" + strconv.Quote(sp.Service), "kind=" + strconv.Quote(sp.Kind)}
		if m := oneOf("span_name", sp.SpanName); m != "" {
			matchers = append(matchers, m)
		}
		total = fmt.Sprintf("sum(increase(traces_spans_total{%s}[1m]))", strings.Join(matchers, ", "))
		ok := append(matchers, `status="ok"`)
		if sp.LatencyBelow != 0 {
			le, _ := bucketFor(time.Duration(sp.LatencyBelow))
			good = fmt.Sprintf("sum(increase(traces_span_duration_seconds_bucket{%s, le=%q}[1m]))", strings.Join(ok, ", "), le)
		} else {
			good = fmt.Sprintf("sum(increase(traces_spans_total{%s}[1m]))", strings.Join(ok, ", "))
		}
	default:
		r := ind.Ratio
		total = fmt.Sprintf("sum(increase(%s[1m]))", strings.TrimSpace(r.Total))
		if r.Good != "" {
			good = fmt.Sprintf("sum(increase(%s[1m]))", strings.TrimSpace(r.Good))
		} else {
			good = fmt.Sprintf("%s - (sum(increase(%s[1m])) or vector(0))", total, strings.TrimSpace(r.Bad))
		}
	}
	// A minute with events but no good ones has a good count of 0, not
	// none; a minute without events has neither.
	good = fmt.Sprintf("((%s) or vector(0)) and on() %s", good, total)
	return good, total
}

// EventsQuery counts the events over d instead of a minute, to tell
// whether the indicator has any data at all. The selectors in an SLO hold
// no brackets (Check refuses them), so every "[1m]" is one of ours.
func (s *SLO) EventsQuery(d time.Duration) string {
	_, total := s.Queries()
	return strings.ReplaceAll(total, "[1m]", "["+model.Duration(d).String()+"]")
}

// Source names what the indicator reads, for messages.
func (s *SLO) Source() string {
	switch ind := s.Indicator; {
	case ind.Probe != nil:
		return fmt.Sprintf("the probe %q", ind.Probe.Name)
	case ind.Span != nil:
		return fmt.Sprintf("traces of the service %q (kind %s)", ind.Span.Service, ind.Span.Kind)
	default:
		return strings.TrimSpace(ind.Ratio.Total)
	}
}

// selector writes name{key="value", list=~"a|b"}.
func selector(metric, key, value, listKey string, list []string) string {
	m := []string{key + "=" + strconv.Quote(value)}
	if l := oneOf(listKey, list); l != "" {
		m = append(m, l)
	}
	return metric + "{" + strings.Join(m, ", ") + "}"
}

// oneOf matches a label against a list of exact values.
func oneOf(key string, values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return key + "=" + strconv.Quote(values[0])
	}
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = regexp.QuoteMeta(v)
	}
	return key + "=~" + strconv.Quote(strings.Join(q, "|"))
}

// SeriesLabels are the labels on the SLO's recorded series.
func (s *SLO) SeriesLabels() map[string]string {
	l := map[string]string{"slo": s.Name}
	if s.Service != "" {
		l["service"] = s.Service
	}
	if s.Owner != "" {
		l["owner"] = s.Owner
	}
	for k, v := range s.Labels {
		l[k] = v
	}
	return l
}

// RuleGroup is what the server evaluates for this SLO: the two counts and
// the objective, once a minute.
func (s *SLO) RuleGroup() rules.Group {
	good, total := s.Queries()
	return rules.Group{
		Name:     GroupPrefix + s.Name,
		Interval: model.Duration(time.Minute),
		Delay:    Delay,
		Labels:   s.SeriesLabels(),
		Rules: []rules.Rule{
			{Record: GoodMetric, Expr: good},
			{Record: TotalMetric, Expr: total},
			// The objective as a ratio, so dashboards and alerts can join on it.
			{Record: ObjectiveMetric, Expr: fmt.Sprintf("vector(%s)", strconv.FormatFloat(s.Ratio(), 'g', -1, 64))},
		},
	}
}
