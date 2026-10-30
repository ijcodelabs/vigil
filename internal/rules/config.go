// Package rules evaluates alerting and recording rules against the metric
// and log stores. The file format is Prometheus' rule-file format, with one
// addition: a group may set `type: logs` to make its expressions LogsQL.
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v3"
)

const (
	// AlignDelay is the Delay for groups recording per-minute amounts: the
	// metric store hides its last 30 seconds, and agents push every 15
	// seconds or so.
	AlignDelay = 90 * time.Second

	TypeMetrics     = "metrics"
	TypeLogs        = "logs"
	defaultInterval = time.Minute
)

type File struct {
	Groups []Group `yaml:"groups"`
}

type Group struct {
	Name     string            `yaml:"name"`
	Type     string            `yaml:"type"` // metrics (default) or logs
	Interval model.Duration    `yaml:"interval"`
	Labels   map[string]string `yaml:"labels"`
	Rules    []Rule            `yaml:"rules"`
	// Delay, set by the server for the groups it generates (SLOs), makes
	// the group record whole intervals: each one once, at its end, Delay
	// after it ended, so late samples are in and a restart does not record
	// an interval twice. Meant for recording rules. Not in the file format.
	Delay time.Duration `yaml:"-" json:"-"`
}

type Rule struct {
	Alert         string            `yaml:"alert"`
	Record        string            `yaml:"record"`
	Expr          string            `yaml:"expr"`
	For           model.Duration    `yaml:"for"`
	KeepFiringFor model.Duration    `yaml:"keep_firing_for"`
	Labels        map[string]string `yaml:"labels"`
	Annotations   map[string]string `yaml:"annotations"`
}

func (g *Group) interval() time.Duration {
	if g.Interval > 0 {
		return time.Duration(g.Interval)
	}
	return defaultInterval
}

func (r *Rule) name() string {
	if r.Alert != "" {
		return r.Alert
	}
	return r.Record
}

// key identifies a rule across reloads, so its alerts keep their state when
// unrelated parts of the file change.
func (g *Group) key(r *Rule) string { return g.Name + "\x00" + r.name() + "\x00" + r.Expr }

// Parse reads and statically validates a rule file. Unknown fields are
// errors, so a typo cannot silently disable a rule. Expressions are checked
// later against the stores (Manager.Check).
func Parse(text string) (*File, error) {
	f := &File{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	seen := map[string]bool{}
	for gi := range f.Groups {
		g := &f.Groups[gi]
		if g.Name == "" {
			return nil, fmt.Errorf("group %d: name is required", gi+1)
		}
		if seen[g.Name] {
			return nil, fmt.Errorf("group %q is defined twice", g.Name)
		}
		seen[g.Name] = true
		switch g.Type {
		case "":
			g.Type = TypeMetrics
		case TypeMetrics, TypeLogs:
		default:
			return nil, fmt.Errorf("group %q: type must be %q or %q", g.Name, TypeMetrics, TypeLogs)
		}
		if err := checkLabels(g.Labels); err != nil {
			return nil, fmt.Errorf("group %q: %w", g.Name, err)
		}
		for ri := range g.Rules {
			r := &g.Rules[ri]
			where := fmt.Sprintf("group %q, rule %d", g.Name, ri+1)
			if r.name() != "" {
				where = fmt.Sprintf("group %q, rule %q", g.Name, r.name())
			}
			if (r.Alert == "") == (r.Record == "") {
				return nil, fmt.Errorf("%s: set exactly one of alert and record", where)
			}
			if r.Expr == "" {
				return nil, fmt.Errorf("%s: expr is required", where)
			}
			if err := checkLabels(r.Labels); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if r.Record != "" {
				if !model.LegacyValidation.IsValidMetricName(r.Record) {
					return nil, fmt.Errorf("%s: %q is not a valid metric name", where, r.Record)
				}
				if r.For != 0 || r.KeepFiringFor != 0 || len(r.Annotations) > 0 {
					return nil, fmt.Errorf("%s: for, keep_firing_for and annotations only apply to alerts", where)
				}
				continue
			}
			for _, m := range []map[string]string{r.Labels, r.Annotations} {
				for k, v := range m {
					if _, err := compile(v); err != nil {
						return nil, fmt.Errorf("%s: template in %q: %w", where, k, err)
					}
				}
			}
		}
	}
	return f, nil
}

func checkLabels(m map[string]string) error {
	for k := range m {
		if !model.LegacyValidation.IsValidLabelName(k) || k == "alertname" || k == model.MetricNameLabel {
			return fmt.Errorf("%q cannot be used as a label name", k)
		}
	}
	return nil
}
