package alerting

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/pkg/labels"
	"github.com/prometheus/alertmanager/silence"
	pb "github.com/prometheus/alertmanager/silence/silencepb"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"
)

// Alertmanager says nothing about a silenced alert: not that it was
// silenced, and not that it ended. Two messages fill that gap, sent to the
// contacts the alert is routed to:
//
//   - when a silence is made: who silenced what, until when, and why;
//   - when an alert ends while silenced, but only to contacts that had been
//     told it was firing. One that began and ended inside a silence was
//     never announced, so its end is not either.
//
// Both go straight to the receivers' integrations, past grouping, silences
// and inhibition, which would hold them back.

const (
	// SilencedAlertName is the alert name of the message about a new silence.
	SilencedAlertName = "AlertsSilenced"
	directTimeout     = 30 * time.Second
)

// routing is what a configuration turns into; replaced as a whole by Apply.
type routing struct {
	routes    *dispatch.Route
	receivers map[string][]notify.Integration
}

// told remembers which contacts were told that an alert fires, so that its
// end can be reported to exactly those. It is kept in memory: after a
// restart of the server, alerts that end while silenced do so quietly.
type told struct {
	mu sync.Mutex
	at map[string]map[model.Fingerprint]time.Time // receiver -> alert -> when
}

func (t *told) record(receiver string, alerts []*types.Alert) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.at == nil {
		t.at = map[string]map[model.Fingerprint]time.Time{}
	}
	if t.at[receiver] == nil {
		t.at[receiver] = map[model.Fingerprint]time.Time{}
	}
	now := time.Now()
	for _, a := range alerts {
		if a.Resolved() {
			delete(t.at[receiver], a.Fingerprint())
		} else {
			t.at[receiver][a.Fingerprint()] = now
		}
	}
	for fp, when := range t.at[receiver] { // what never ended is forgotten with the alert store's own memory
		if now.Sub(when) > retention {
			delete(t.at[receiver], fp)
		}
	}
}

// take returns the receivers that were told about the alert and forgets
// them, so that a resolution sent several times is reported once.
func (t *told) take(fp model.Fingerprint) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for receiver, alerts := range t.at {
		if _, ok := alerts[fp]; ok {
			delete(alerts, fp)
			out = append(out, receiver)
		}
	}
	sort.Strings(out)
	return out
}

// recording wraps a receiver's integration to note what it delivered.
type recording struct {
	inner    notify.Integration
	receiver string
	told     *told
}

func (r *recording) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	retry, err := r.inner.Notify(ctx, alerts...)
	if err == nil {
		r.told.record(r.receiver, alerts)
	}
	return retry, err
}

func (r *recording) SendResolved() bool { return r.inner.SendResolved() }

func (a *Alertmanager) recordDeliveries(receiver string, in []notify.Integration) []notify.Integration {
	out := make([]notify.Integration, len(in))
	for i := range in {
		r := &recording{inner: in[i], receiver: receiver, told: &a.told}
		out[i] = notify.NewIntegration(r, r, in[i].Name(), in[i].Index(), receiver)
	}
	return out
}

// deliver sends alerts through every integration of the named receivers.
func (a *Alertmanager) deliver(key string, receivers []string, resolved bool, alerts ...*types.Alert) {
	rt := a.routing.Load()
	if rt == nil || len(alerts) == 0 {
		return
	}
	for _, name := range receivers {
		for _, in := range rt.receivers[name] {
			if resolved && !in.SendResolved() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), directTimeout)
			ctx = notify.WithReceiverName(ctx, name)
			ctx = notify.WithGroupKey(ctx, key)
			ctx = notify.WithGroupLabels(ctx, alerts[0].Labels)
			ctx = notify.WithNow(ctx, time.Now())
			ctx = notify.WithRepeatInterval(ctx, 0)
			if _, err := in.Notify(ctx, alerts...); err != nil {
				a.log.Warn("notification about a silence was not delivered", "receiver", name, "integration", in.Name(), "err", err)
			}
			cancel()
		}
	}
}

func matcherSets(s *pb.Silence) []labels.Matchers {
	sets := s.MatcherSets
	if len(sets) == 0 && len(s.Matchers) > 0 {
		sets = []*pb.MatcherSet{{Matchers: s.Matchers}}
	}
	var out []labels.Matchers
	for _, set := range sets {
		var ms labels.Matchers
		for _, m := range set.Matchers {
			t := map[pb.Matcher_Type]labels.MatchType{pb.Matcher_EQUAL: labels.MatchEqual, pb.Matcher_NOT_EQUAL: labels.MatchNotEqual,
				pb.Matcher_REGEXP: labels.MatchRegexp, pb.Matcher_NOT_REGEXP: labels.MatchNotRegexp}[m.Type]
			if lm, err := labels.NewMatcher(t, m.Name, m.Pattern); err == nil {
				ms = append(ms, lm)
			}
		}
		out = append(out, ms)
	}
	return out
}

func describe(sets []labels.Matchers) string {
	var parts []string
	for _, ms := range sets {
		var one []string
		for _, m := range ms {
			one = append(one, m.String())
		}
		parts = append(parts, strings.Join(one, ", "))
	}
	return strings.Join(parts, " or ")
}

// NotifySilence tells the contacts concerned that a silence was made. Those
// are the contacts of the alerts it silences right now; if it silences none
// yet (planned work), the contacts an alert with the silence's own labels
// would reach.
func (a *Alertmanager) NotifySilence(ctx context.Context, id string) {
	rt := a.routing.Load()
	if rt == nil || !a.notifySilences.Load() {
		return
	}
	sil, err := a.silences.QueryOne(ctx, silence.QIDs(id))
	if err != nil || sil == nil {
		a.log.Warn("a silence was made but cannot be read back; nobody was told", "id", id, "err", err)
		return
	}
	sets := matcherSets(sil)
	if len(sets) == 0 {
		return
	}
	covers := func(l model.LabelSet) bool {
		for _, ms := range sets {
			if ms.Matches(l) {
				return true
			}
		}
		return false
	}
	receivers := map[string]bool{}
	covered := 0
	it := a.alerts.GetPending()
	for al := range it.Next() {
		if al.Data == nil || al.Data.Resolved() || !covers(al.Data.Labels) {
			continue
		}
		covered++
		for _, r := range rt.routes.Match(al.Data.Labels) {
			receivers[r.RouteOpts.Receiver] = true
		}
	}
	it.Close()

	// The labels the silence names outright say what it is about, and are
	// what the message is routed by when no alert is covered yet.
	about := model.LabelSet{}
	for _, m := range sets[0] {
		if m.Type == labels.MatchEqual && m.Name != model.AlertNameLabel {
			about[model.LabelName(m.Name)] = model.LabelValue(m.Value)
		}
	}
	if len(receivers) == 0 {
		asAlert := about.Clone()
		for _, m := range sets[0] {
			if m.Type == labels.MatchEqual && m.Name == model.AlertNameLabel {
				asAlert[model.AlertNameLabel] = model.LabelValue(m.Value)
			}
		}
		for _, r := range rt.routes.Match(asAlert) {
			receivers[r.RouteOpts.Receiver] = true
		}
	}
	names := make([]string, 0, len(receivers))
	for r := range receivers {
		names = append(names, r)
	}
	sort.Strings(names)

	ends := sil.EndsAt.AsTime()
	who := sil.CreatedBy
	if who == "" {
		who = "someone"
	}
	summary := fmt.Sprintf("%s silenced %s until %s (%s)", who, describe(sets), ends.UTC().Format("2006-01-02 15:04 MST"),
		model.Duration(time.Until(ends).Round(time.Minute)))
	if sil.Comment != "" {
		summary += ": " + sil.Comment
	}
	switch covered {
	case 0:
		summary += ". No alert is covered right now."
	case 1:
		summary += ". 1 firing alert is covered."
	default:
		summary += fmt.Sprintf(". %d firing alerts are covered.", covered)
	}
	msg := &types.Alert{UpdatedAt: time.Now()}
	msg.Labels = about
	msg.Labels[model.AlertNameLabel] = SilencedAlertName
	msg.Labels["severity"] = "info"
	msg.Annotations = model.LabelSet{"summary": model.LabelValue(summary)}
	msg.StartsAt = time.Now()
	msg.GeneratorURL = strings.TrimSuffix(a.externalURL.String(), RoutePrefix) + "/#/alerts/silences"
	go a.deliver("silence/"+id, names, false, msg)
}

// reportEndedWhileSilenced tells the contacts that had been told an alert
// fires that it has ended, when a silence keeps Alertmanager from doing so.
func (a *Alertmanager) reportEndedWhileSilenced(ctx context.Context, alerts []*types.Alert) {
	if !a.notifySilences.Load() {
		return
	}
	for _, al := range alerts {
		if !al.Resolved() || !a.silencer.Mutes(ctx, al.Labels) {
			continue
		}
		receivers := a.told.take(al.Fingerprint())
		if len(receivers) == 0 {
			continue
		}
		note := "It ended while silenced."
		if sils, _, err := a.silences.Query(ctx, silence.QMatches(al.Labels), silence.QState(silence.SilenceStateActive)); err == nil && len(sils) > 0 {
			note = "It ended while silenced by " + sils[0].CreatedBy
			if sils[0].Comment != "" {
				note += " (" + sils[0].Comment + ")"
			}
			note += "."
		}
		out := *al
		out.Annotations = al.Annotations.Clone()
		out.Annotations["silence"] = model.LabelValue(note)
		if s, ok := out.Annotations["summary"]; ok {
			out.Annotations["summary"] = s + model.LabelValue(" — "+note)
		}
		go a.deliver("ended-while-silenced/"+al.Fingerprint().String(), receivers, true, &out)
	}
}
