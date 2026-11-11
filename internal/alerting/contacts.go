package alerting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"
)

// A contact that is wrong is found when an alert does not arrive. These
// let an admin find it first: a test message through one integration of a
// contact, and which contacts an alert with given labels would reach.
// Both work on the configuration in use or on one that is not saved yet.

// TestAlertName is the alertname of a test message.
const TestAlertName = "VigilTest"

// testGap is how long one integration waits between two tests, so a button
// pressed again and again does not flood a channel.
const testGap = 10 * time.Second

// Target names one integration of a contact: its receiver, its kind and
// which of that kind it is (0 for the first).
type Target struct {
	Receiver    string `json:"receiver"`
	Integration string `json:"integration"`
	Index       int    `json:"index"`
}

func (t Target) key() string {
	return fmt.Sprintf("%s\x00%s\x00%d", t.Receiver, t.Integration, t.Index)
}

var (
	// ErrNoIntegration is returned for a target the configuration does not have.
	ErrNoIntegration = errors.New("no such contact or integration")
	// ErrTooSoon is returned for a second test of the same integration within testGap.
	ErrTooSoon = errors.New("this integration was tested a moment ago; wait a few seconds")
)

type testLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *testLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[key]; ok && now.Sub(t) < testGap {
		return false
	}
	l.last[key] = now
	return true
}

// SendTest sends one test message through one integration, past routing,
// grouping, inhibition and silences, and returns what that integration
// answered. With yaml empty it uses the configuration in use, and the
// attempt counts in Deliveries like any other; with a configuration that
// is not saved yet, nothing is recorded. by is who asked, for the message.
func (a *Alertmanager) SendTest(ctx context.Context, yaml string, t Target, by string) error {
	var integrations []notify.Integration
	if strings.TrimSpace(yaml) == "" {
		if rt := a.routing.Load(); rt != nil {
			integrations = rt.receivers[t.Receiver]
		}
	} else {
		_, _, _, built, err := a.compile(yaml, true)
		if err != nil {
			return err
		}
		integrations = built[t.Receiver]
	}
	var in *notify.Integration
	for i := range integrations {
		if integrations[i].Name() == t.Integration && integrations[i].Index() == t.Index {
			in = &integrations[i]
		}
	}
	if in == nil {
		return ErrNoIntegration
	}
	now := time.Now()
	if !a.tests.allow(t.key(), now) {
		return ErrTooSoon
	}

	who := "an admin"
	if by != "" {
		who = by
	}
	al := &types.Alert{UpdatedAt: now}
	al.Labels = model.LabelSet{"alertname": TestAlertName, "severity": "info", "receiver": model.LabelValue(t.Receiver)}
	al.Annotations = model.LabelSet{
		"summary": model.LabelValue(fmt.Sprintf("Test message from vigil to %s (%s), sent by %s. Nothing is wrong.", t.Receiver, t.Integration, who)),
	}
	al.StartsAt, al.EndsAt = now, now.Add(5*time.Minute)

	ctx, cancel := context.WithTimeout(ctx, directTimeout)
	defer cancel()
	ctx = notify.WithReceiverName(ctx, t.Receiver)
	ctx = notify.WithGroupKey(ctx, "vigil-test/"+t.key())
	ctx = notify.WithGroupLabels(ctx, al.Labels)
	ctx = notify.WithNow(ctx, now)
	ctx = notify.WithRepeatInterval(ctx, 0)
	ctx = notify.WithNotificationReason(ctx, notify.ReasonFirstNotification)
	if _, err := in.Notify(ctx, al); err != nil {
		return errors.New(cleanError(err)) // never the address or token it failed to reach
	}
	return nil
}

// RouteMatch is one contact an alert would reach, and on which route.
type RouteMatch struct {
	Receiver       string   `json:"receiver"`
	Route          string   `json:"route"` // the route's matchers from the top, "{}" for the top route
	GroupBy        []string `json:"group_by"`
	GroupWait      string   `json:"group_wait"`
	GroupInterval  string   `json:"group_interval"`
	RepeatInterval string   `json:"repeat_interval"`
	TimeIntervals  []string `json:"time_intervals,omitempty"` // mute and active intervals the route names
	Integrations   []Target `json:"integrations"`
}

// RouteResult is a dry run of the routing for one set of labels.
type RouteResult struct {
	Matches  []RouteMatch `json:"matches"`
	Silenced bool         `json:"silenced"` // a silence in effect now covers these labels
}

// Route says which contacts an alert with these labels would be sent to,
// without sending anything. With yaml empty it uses the configuration in
// use. Inhibition is not considered: it depends on the other alerts firing.
func (a *Alertmanager) Route(ctx context.Context, yaml string, labels map[string]string) (*RouteResult, error) {
	var routes *dispatch.Route
	var receivers map[string][]notify.Integration
	if strings.TrimSpace(yaml) == "" {
		rt := a.routing.Load()
		if rt == nil {
			return nil, errors.New("no configuration is in use yet")
		}
		routes, receivers = rt.routes, rt.receivers
	} else {
		var err error
		if _, _, routes, receivers, err = a.compile(yaml, false); err != nil {
			return nil, err
		}
	}
	lset := model.LabelSet{}
	for k, v := range labels {
		name := model.LabelName(k)
		if !name.IsValid() {
			return nil, fmt.Errorf("%q is not a valid label name", k)
		}
		lset[name] = model.LabelValue(v)
	}

	out := &RouteResult{Matches: []RouteMatch{}, Silenced: a.silencer.Mutes(ctx, lset)}
	for _, r := range routes.Match(lset) {
		o := r.RouteOpts
		m := RouteMatch{Receiver: o.Receiver, Route: r.Key(), GroupWait: o.GroupWait.String(),
			GroupInterval: o.GroupInterval.String(), RepeatInterval: o.RepeatInterval.String(), Integrations: []Target{}}
		if o.GroupByAll {
			m.GroupBy = []string{"..."}
		} else {
			for l := range o.GroupBy {
				m.GroupBy = append(m.GroupBy, string(l))
			}
			sort.Strings(m.GroupBy)
		}
		m.TimeIntervals = append(append(m.TimeIntervals, o.MuteTimeIntervals...), o.ActiveTimeIntervals...)
		for _, in := range receivers[o.Receiver] {
			m.Integrations = append(m.Integrations, Target{Receiver: o.Receiver, Integration: in.Name(), Index: in.Index()})
		}
		out.Matches = append(out.Matches, m)
	}
	return out, nil
}
