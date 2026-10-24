// Package alerting embeds Alertmanager: grouping, routing, inhibition,
// silences and the notifiers (email, Slack, webhook, PagerDuty, ...). The
// wiring mirrors Alertmanager's own app package, minus clustering and UI,
// so its configuration format and API v2 work unchanged.
package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/alertmanager/api"
	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/config/receiver"
	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/eventrecorder"
	"github.com/prometheus/alertmanager/featurecontrol"
	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/marker"
	"github.com/prometheus/alertmanager/matcher/compat"
	"github.com/prometheus/alertmanager/nflog"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/provider/mem"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/timeinterval"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"
)

const (
	// RoutePrefix is where the Alertmanager API is mounted on the server.
	RoutePrefix = "/alertmanager"

	retention   = 120 * time.Hour
	maintenance = 15 * time.Minute

	// DefaultConfig delivers nothing until contacts are configured.
	DefaultConfig = `route:
  receiver: none
receivers:
  - name: none
`
)

// Alert is one firing or resolved alert handed over by the rule evaluator.
type Alert struct {
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	EndsAt       time.Time
	GeneratorURL string
}

type Alertmanager struct {
	log         *slog.Logger
	externalURL *url.URL
	started     time.Time
	handler     http.Handler

	alerts      *mem.Alerts
	silences    *silence.Silences
	silencer    *silence.Silencer
	nflog       *nflog.Log
	groupMarker marker.GroupMarker
	pipeline    *notify.PipelineBuilder
	dispMetrics *dispatch.DispatcherMetrics
	api         *api.API
	events      eventrecorder.Recorder

	// See silences.go: who is told about silences, and what they were told before.
	routing        atomic.Pointer[routing]
	told           told
	notifySilences atomic.Bool

	mu         sync.Mutex // serialises Apply
	dispatcher atomic.Pointer[dispatch.Dispatcher]
	inhibitor  atomic.Pointer[inhibit.Inhibitor]

	stopc chan struct{}
	wg    sync.WaitGroup
}

var compatOnce sync.Once

// Validate checks an Alertmanager configuration without applying it.
func Validate(yaml string) error {
	_, err := config.Load(yaml)
	return err
}

// New starts an Alertmanager that keeps silences and its notification log
// under dataDir. Call Apply with a configuration before sending alerts.
func New(dataDir, serverURL string, log *slog.Logger) (*Alertmanager, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	ext, err := url.Parse(strings.TrimRight(serverURL, "/") + RoutePrefix)
	if err != nil {
		return nil, err
	}
	ff, err := featurecontrol.NewFlags(log, "")
	if err != nil {
		return nil, err
	}
	compatOnce.Do(func() { compat.InitFromFlags(log, ff) })
	reg := prometheus.NewRegistry() // private: nothing leaks into a global registry

	a := &Alertmanager{log: log, externalURL: ext, started: time.Now(), stopc: make(chan struct{}),
		groupMarker: marker.NewGroupMarker()}

	nflogFile, silFile := filepath.Join(dataDir, "nflog"), filepath.Join(dataDir, "silences")
	if a.nflog, err = nflog.New(nflog.Options{SnapshotFile: nflogFile, Retention: retention,
		Logger: log.With("component", "nflog"), Metrics: reg}); err != nil {
		return nil, fmt.Errorf("notification log: %w", err)
	}
	silences, err := silence.New(silence.Options{SnapshotFile: silFile, Retention: retention,
		Limits: silence.Limits{MaxSilences: func() int { return 0 }, MaxSilenceSizeBytes: func() int { return 0 }},
		Logger: log.With("component", "silences"), Metrics: reg, EventRecorder: a.events})
	if err != nil {
		return nil, fmt.Errorf("silences: %w", err)
	}
	a.wg.Go(func() { a.nflog.Maintenance(maintenance, nflogFile, a.stopc, nil) })
	a.wg.Go(func() { silences.Maintenance(maintenance, silFile, a.stopc, nil) })

	a.silences = silences
	a.notifySilences.Store(true)
	a.silencer = silence.NewSilencer(silences, log, a.events)
	if a.alerts, err = mem.NewAlerts(context.Background(), 30*time.Minute, 0, a.silencer, log, a.events, reg, ff); err != nil {
		return nil, fmt.Errorf("alert store: %w", err)
	}
	a.pipeline = notify.NewPipelineBuilder(reg, ff, a.events)
	a.dispMetrics = dispatch.NewDispatcherMetrics(false, reg, ff)

	a.api, err = api.New(api.Options{
		Alerts:         a.alerts,
		Silences:       silences,
		GroupMutedFunc: a.groupMarker.Muted,
		Logger:         log.With("component", "api"),
		Registry:       reg,
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "alertmanager_http_request_duration_seconds", Help: "Latency of Alertmanager API requests.",
		}, []string{"handler", "method", "code"}),
		GroupFunc: func(ctx context.Context, rf func(*dispatch.Route) bool, af func(*types.Alert, time.Time) bool) (dispatch.AlertGroups, map[model.Fingerprint][]string, error) {
			return a.dispatcher.Load().Groups(ctx, rf, af)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("alertmanager api: %w", err)
	}
	a.handler = a.api.Register(route.New().WithPrefix(RoutePrefix), RoutePrefix)
	return a, nil
}

// Handler serves the Alertmanager API v2 under RoutePrefix.
func (a *Alertmanager) Handler() http.Handler { return a.handler }

// Apply validates and activates a configuration, replacing the previous one.
func (a *Alertmanager) Apply(yaml string) error {
	conf, err := config.Load(yaml)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	tmpl, err := template.FromGlobs(conf.Templates)
	if err != nil {
		return fmt.Errorf("notification templates: %w", err)
	}
	tmpl.ExternalURL = a.externalURL

	routes := dispatch.NewRoute(conf.Route, nil)
	active := map[string]struct{}{}
	routes.Walk(func(rt *dispatch.Route) { active[rt.RouteOpts.Receiver] = struct{}{} })
	receivers := map[string][]notify.Integration{}
	for _, rcv := range conf.Receivers {
		if _, used := active[rcv.Name]; !used {
			continue
		}
		integrations, err := receiver.BuildReceiverIntegrations(rcv, tmpl, a.log)
		if err != nil {
			return err
		}
		receivers[rcv.Name] = a.recordDeliveries(rcv.Name, integrations)
	}
	intervals := map[string][]timeinterval.TimeInterval{}
	for _, ti := range conf.MuteTimeIntervals {
		intervals[ti.Name] = ti.TimeIntervals
	}
	for _, ti := range conf.TimeIntervals {
		intervals[ti.Name] = ti.TimeIntervals
	}

	// Everything that can fail is done; swap the running pieces.
	a.stopRouting()
	inhibitor := inhibit.NewInhibitor(a.alerts, conf.InhibitRules, a.log, a.events)
	noWait := func() time.Duration { return 0 }
	pipeline := a.pipeline.New(receivers, noWait, inhibitor, a.silencer,
		timeinterval.NewIntervener(intervals), a.groupMarker, a.nflog, nil)
	a.api.Update(conf, func(ctx context.Context, labels model.LabelSet) {
		a.inhibitor.Load().Mutes(ctx, labels)
		a.silencer.Mutes(ctx, labels)
	})
	timeout := func(d time.Duration) time.Duration { return max(d, notify.MinTimeout) }
	dispatcher := dispatch.NewDispatcher(a.alerts, routes, pipeline, a.groupMarker, timeout,
		30*time.Second, nil, a.log, a.events, a.dispMetrics, tmpl)

	go inhibitor.Run()
	inhibitor.WaitForLoading()
	a.inhibitor.Store(inhibitor)
	go dispatcher.Run(a.started)
	dispatcher.WaitForLoading()
	a.dispatcher.Store(dispatcher)
	a.routing.Store(&routing{routes: routes, receivers: receivers})
	return nil
}

// NotifyAboutSilences switches the two messages about silences on or off
// (on by default): that one was made, and that an alert people had been
// told about ended while silenced.
func (a *Alertmanager) NotifyAboutSilences(on bool) { a.notifySilences.Store(on) }

func (a *Alertmanager) stopRouting() {
	if i := a.inhibitor.Load(); i != nil {
		i.Stop()
	}
	if d := a.dispatcher.Load(); d != nil {
		d.Stop()
	}
}

// Send hands alerts to Alertmanager. An alert whose EndsAt is in the past
// is a resolution.
func (a *Alertmanager) Send(ctx context.Context, alerts []Alert) error {
	now := time.Now()
	out := make([]*types.Alert, 0, len(alerts))
	for _, in := range alerts {
		al := &types.Alert{UpdatedAt: now}
		al.Labels, al.Annotations = model.LabelSet{}, model.LabelSet{}
		for k, v := range in.Labels {
			al.Labels[model.LabelName(k)] = model.LabelValue(v)
		}
		for k, v := range in.Annotations {
			al.Annotations[model.LabelName(k)] = model.LabelValue(v)
		}
		al.StartsAt, al.EndsAt, al.GeneratorURL = in.StartsAt, in.EndsAt, in.GeneratorURL
		if err := al.Validate(); err != nil {
			return fmt.Errorf("alert %v: %w", in.Labels, err)
		}
		out = append(out, al)
	}
	if err := a.alerts.Put(ctx, out...); err != nil {
		return err
	}
	a.reportEndedWhileSilenced(ctx, out)
	return nil
}

// ActiveAlert is an alert that has not ended, as the incident view sees it.
type ActiveAlert struct {
	Alert
	Silenced bool
}

// Active lists the alerts that have not ended, rule alerts and detections
// alike, with whether a silence covers each. The second result is how many
// silences are in effect.
func (a *Alertmanager) Active(ctx context.Context) ([]ActiveAlert, int, error) {
	var out []ActiveAlert
	it := a.alerts.GetPending()
	defer it.Close()
	for al := range it.Next() {
		if al.Data == nil || al.Data.Resolved() {
			continue
		}
		in := ActiveAlert{Alert: Alert{Labels: map[string]string{}, Annotations: map[string]string{},
			StartsAt: al.Data.StartsAt, EndsAt: al.Data.EndsAt, GeneratorURL: al.Data.GeneratorURL}}
		for k, v := range al.Data.Labels {
			in.Labels[string(k)] = string(v)
		}
		for k, v := range al.Data.Annotations {
			in.Annotations[string(k)] = string(v)
		}
		in.Silenced = a.silencer.Mutes(ctx, al.Data.Labels)
		out = append(out, in)
	}
	if err := it.Err(); err != nil {
		return nil, 0, err
	}
	n, err := a.silences.CountState(ctx, silence.SilenceStateActive)
	return out, n, err
}

// Close stops routing and writes silences and the notification log to disk.
func (a *Alertmanager) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopRouting()
	close(a.stopc)
	a.wg.Wait()
	a.alerts.Close()
}
