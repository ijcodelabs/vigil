// Package agent collects on a host and pushes to the central server.
package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/collect"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/discovery"
	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/facts"
	"github.com/ijcodelabs/vigil/internal/gpu"
	"github.com/ijcodelabs/vigil/internal/inventory"
	"github.com/ijcodelabs/vigil/internal/kube"
	"github.com/ijcodelabs/vigil/internal/logs"
	"github.com/ijcodelabs/vigil/internal/logwire"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/otlp"
	"github.com/ijcodelabs/vigil/internal/probe"
	"github.com/ijcodelabs/vigil/internal/procmetrics"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/redact"
	"github.com/ijcodelabs/vigil/internal/spool"
	"github.com/ijcodelabs/vigil/internal/systemd"
)

type Agent struct {
	cfg     *config.Agent
	version string
	log     *slog.Logger
	http    *http.Client
	// ready is closed once the client trusts the server and the agent has
	// its token; nothing is sent before that.
	ready    chan struct{}
	started  time.Time
	metrics  *shipper
	logs     *shipper
	events   *shipper // detections from runtime-event rules
	captures *shipper
	traces   *shipper // spans kept for the trace store, as OTLP

	// PollEvery is how often the managed config is checked.
	PollEvery time.Duration

	mu         sync.Mutex
	remoteHash string // hash of the managed config in use; "" when running the local file
	remoteErr  string // why the last managed config could not be used
	eventsErr  string // why runtime events are off although configured
	// images lists the images of running containers, when the current
	// config has Docker switched on.
	images func() []string

	limits limiter
}

// shipper is one signal's disk queue and the endpoint it drains to.
type shipper struct {
	name     string
	spool    *spool.Spool
	path     string
	encoding string
	headers  map[string]string
	wake     chan struct{}
	// split, if set, separates a spooled item into per-request headers and
	// the body to send.
	split func([]byte) (map[string]string, []byte)
}

func New(cfg *config.Agent, version string, log *slog.Logger) (*Agent, error) {
	a := &Agent{cfg: cfg, version: version, log: log, http: &http.Client{Timeout: 30 * time.Second},
		PollEvery: 30 * time.Second, ready: make(chan struct{}), started: time.Now()}
	if cfg.LocalLabels == nil { // built in code rather than loaded from a file
		cfg.LocalLabels = maps.Clone(cfg.Labels)
	}
	open := func(name, path, encoding string, headers map[string]string, budget int64) (*shipper, error) {
		sp, err := spool.Open(filepath.Join(cfg.SpoolDir, name), budget)
		if err != nil {
			return nil, fmt.Errorf("spool: %w", err)
		}
		return &shipper{name: name, spool: sp, path: path, encoding: encoding, headers: headers,
			wake: make(chan struct{}, 1)}, nil
	}
	var err error
	if a.metrics, err = open("metrics", "/api/v1/write", "snappy", map[string]string{
		"Content-Type": "application/x-protobuf", "X-Prometheus-Remote-Write-Version": "0.1.0"}, cfg.SpoolMaxBytes/2); err != nil {
		return nil, err
	}
	// Metrics and logs share the configured budget; detections and captures
	// are small and rare and get fixed ones of their own.
	ndjson := map[string]string{"Content-Type": "application/x-ndjson"}
	if a.logs, err = open("logs", "/api/v1/logs", "gzip", ndjson, cfg.SpoolMaxBytes/2); err != nil {
		return nil, err
	}
	if a.events, err = open("events", "/api/v1/events", "gzip", ndjson, 16<<20); err != nil {
		return nil, err
	}
	if a.captures, err = open("captures", "/api/v1/captures", "gzip", map[string]string{"Content-Type": "application/json"}, 256<<20); err != nil {
		return nil, err
	}
	a.captures.split = splitCapture
	if a.traces, err = open("traces", "/api/v1/traces", "gzip", map[string]string{"Content-Type": "application/x-protobuf"}, 128<<20); err != nil {
		return nil, err
	}
	// A mistake in the local file should stop the agent at startup, not
	// leave it running with part of its sources.
	return a, validate(&cfg.Collection)
}

func validate(c *config.Collection) error {
	if err := c.Check(); err != nil {
		return err
	}
	if err := logs.Validate(c); err != nil {
		return err
	}
	return events.Validate(&c.Security)
}

// A spooled capture is its metadata (JSON, length-prefixed) followed by the
// compressed capture; the metadata travels as a header.
func joinCapture(c *events.Capture) []byte {
	meta, _ := json.Marshal(c.Meta)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(meta)))
	return append(append(out, meta...), c.Encode()...)
}

func splitCapture(item []byte) (map[string]string, []byte) {
	if len(item) < 4 || int(binary.BigEndian.Uint32(item)) > len(item)-4 {
		return nil, item
	}
	n := int(binary.BigEndian.Uint32(item))
	return map[string]string{"X-Vigil-Capture": base64.StdEncoding.EncodeToString(item[4 : 4+n])}, item[4+n:]
}

// Run blocks until ctx is cancelled. Unsent batches stay in the spool.
//
// What is collected comes from the local file, unless the server manages a
// config for this agent; that one then replaces it, and is re-applied
// whenever it changes. The last managed config is cached on disk so the
// agent starts with it even when the server is unreachable.
func (a *Agent) Run(ctx context.Context) {
	go a.connect(ctx)
	for _, s := range []*shipper{a.metrics, a.logs, a.events, a.captures, a.traces} {
		go a.sendLoop(ctx, s)
	}

	cur := &a.cfg.Collection
	updates := make(chan *config.Collection, 1)
	if *a.cfg.RemoteConfig {
		cache := filepath.Join(a.cfg.SpoolDir, "managed-config.yaml")
		if text, err := os.ReadFile(cache); err == nil {
			if c, err := a.parseManaged(string(text)); err == nil {
				cur, a.remoteHash = c, config.Hash(string(text))
			}
		}
		go a.pollConfig(ctx, cache, updates)
	}
	for {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); a.collect(runCtx, cur) }()
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case next := <-updates:
			cancel()
			<-done // sources stop and save their positions before the new ones start
			if cur = next; cur == nil {
				cur = &a.cfg.Collection
				a.log.Info("managed config removed, back to the local file")
			} else {
				a.log.Info("managed config applied")
			}
		}
	}
}

// parseManaged reads a config sent by the server. The labels from the
// agent's own file stay on top: they say what this host is.
func (a *Agent) parseManaged(text string) (*config.Collection, error) {
	c, err := config.ParseCollection(text)
	if err != nil {
		return nil, err
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	maps.Copy(c.Labels, a.cfg.LocalLabels)
	return c, validate(c)
}

// collect runs the sources of one collection config until ctx ends.
func (a *Agent) collect(ctx context.Context, col *config.Collection) {
	var docker *dockerapi.Client
	var images func() []string
	if col.Docker.Enabled {
		docker = dockerapi.New(col.Docker.Socket)
		images = func() []string {
			var out []string
			if list, err := docker.List(ctx); err == nil {
				for _, ct := range list {
					if ct.State == "running" {
						out = append(out, ct.Image)
					}
				}
			}
			return out
		}
	}
	a.mu.Lock()
	a.images = images
	a.mu.Unlock()
	// On a Kubernetes node, one client lists the node's pods for everything
	// that needs them: discovery, pod state, log labels, event names.
	var pods *kube.Client
	var podsErr error
	if col.Scrape.Kubernetes.Enabled || col.Scrape.Kubelet.Enabled || col.Logs.Kubernetes.Enabled || col.Security.Enabled {
		node := col.Scrape.Kubernetes.Node
		if node == "" {
			node = os.Getenv("NODE_NAME")
		}
		pods, podsErr = kube.InCluster(node)
	}
	a.limits.configure(col.Limits)
	// Everything collected, from whatever source, passes the limits on its
	// way to the disk queue.
	emitSamples := func(batch []model.Sample) {
		if batch = a.limits.samples(batch); len(batch) == 0 {
			return
		}
		if err := a.enqueue(a.metrics, promwire.Encode(batch)); err != nil {
			a.log.Error("metric batch lost", "samples", len(batch), "err", err)
		}
	}
	// Secrets are masked before the limits count the bytes, so what is
	// counted is what is sent.
	red, err := redact.New(col.Redact)
	if err != nil { // the config was checked when it was loaded
		a.log.Error("redaction rules not usable, only the built-in ones apply", "err", err)
		red, _ = redact.New(config.Redact{Builtin: col.Redact.Builtin})
	}
	emitLogs := func(batch []model.LogEntry) error {
		red.Entries(batch)
		if batch = a.limits.logs(batch); len(batch) == 0 {
			return nil
		}
		return a.enqueue(a.logs, logwire.Encode(batch))
	}
	c := &collect.Collector{
		Interval:    col.Interval.Std(),
		Discoverers: discovery.FromConfig(col.Scrape, pods, podsErr),
		HostMetrics: *col.HostMetrics,
		Labels:      col.Labels,
		Log:         a.log,
		Emit:        emitSamples,
		Sources:     []collect.Source{{Name: "agent", Collect: a.selfMetrics}},
	}
	if docker != nil && *col.Docker.Metrics {
		c.Sources = append(c.Sources, collect.Source{Name: "containers", Collect: docker.Metrics})
	}
	if col.Scrape.Kubelet.Enabled {
		kubelet, err := kube.NewKubelet()
		switch {
		case pods == nil:
			a.log.Warn("kubelet metrics are off", "reason", podsErr)
		case err != nil:
			a.log.Warn("kubelet metrics are off", "reason", err)
		default:
			interval := col.Scrape.Kubelet.Interval.Std()
			c.Sources = append(c.Sources,
				collect.Source{Name: "kubelet", Interval: interval, Collect: kubelet.Collect},
				collect.Source{Name: "pods", Interval: interval, Collect: pods.PodState})
		}
	}
	for _, p := range col.Probes {
		if !config.Off(p.Enabled) {
			c.Sources = append(c.Sources, collect.Source{Name: "probe " + p.Name, Interval: p.Interval.Std(),
				Collect: func(ctx context.Context) ([]model.Sample, error) { return probe.Check(ctx, p), nil }})
		}
	}
	if pm := procmetrics.New(col.Processes); pm.Active() {
		c.Sources = append(c.Sources, collect.Source{Name: "processes", Collect: pm.Collect})
	}
	// GPUs: on wherever one can be read, unless the config says no.
	if g := col.GPU; g.Enabled == nil || *g.Enabled {
		gc := gpu.New()
		gc.Processes = g.Processes == nil || *g.Processes
		gc.ContainerNames = func() map[string]string {
			names := map[string]string{}
			if docker != nil {
				if list, err := docker.List(ctx); err == nil {
					for _, ct := range list {
						names[ct.ID] = ct.Name()
					}
				}
			}
			return names
		}
		switch vendors := gc.Vendors(); {
		case len(vendors) > 0:
			every := max(g.Interval.Std(), col.Interval.Std(), 15*time.Second)
			a.log.Info("GPU metrics on", "vendors", strings.Join(vendors, ","), "every", every)
			c.Sources = append(c.Sources, collect.Source{Name: "gpu", Interval: every, Collect: gc.Collect})
		case g.Enabled != nil:
			a.log.Warn("gpu.enabled is set, but this host has no GPU that can be read (no nvidia-smi, no AMD card in sysfs)")
		}
	}
	if col.Systemd.Enabled {
		sd := systemd.New(col.Systemd)
		unavailable := false
		c.Sources = append(c.Sources, collect.Source{Name: "systemd", Collect: func(ctx context.Context) ([]model.Sample, error) {
			if unavailable {
				return nil, nil
			}
			out, err := sd.Collect(ctx)
			if errors.Is(err, systemd.ErrUnavailable) {
				unavailable = true // say it once, not every interval
				a.log.Warn("systemd unit states are off: systemctl not found on this host")
				return nil, nil
			}
			return out, err
		}})
	}
	logMgr, err := logs.NewManager(col, a.cfg.SpoolDir, docker, pods, emitLogs, a.log)
	if err != nil {
		a.log.Error("log sources not started", "err", err)
	}
	var wg sync.WaitGroup
	if err == nil && logMgr.Active() {
		wg.Go(func() { logMgr.Run(ctx) })
	}
	a.setEventsErr("")
	if col.Security.Enabled {
		mon := events.NewMonitor(&col.Security, a.log)
		// Events carry a container id; Docker or the node's pod list turn
		// it into a name a person recognises.
		mon.ContainerNames = func() map[string]string {
			names := map[string]string{}
			if docker != nil {
				if list, err := docker.List(ctx); err == nil {
					for _, ct := range list {
						names[ct.ID] = ct.Name()
					}
				}
			}
			if pods != nil {
				list, _ := pods.Pods(ctx)
				for _, p := range list {
					for _, ct := range p.Containers {
						if ct.ID != "" {
							names[ct.ID] = p.Namespace + "/" + p.Name + "/" + ct.Name
						}
					}
				}
			}
			return names
		}
		mon.SendDetections = func(ds []events.Detection) error {
			return a.enqueue(a.events, events.EncodeDetections(ds))
		}
		mon.SendCapture = func(cp *events.Capture) error { return a.enqueue(a.captures, joinCapture(cp)) }
		c.Sources = append(c.Sources, collect.Source{Name: "events", Collect: mon.Metrics})
		wg.Go(func() {
			err := mon.Run(ctx)
			switch {
			case errors.Is(err, events.ErrUnsupported):
				// Expected on older kernels and without privileges: say so
				// once and carry on with metrics and logs.
				a.log.Warn("runtime events are off on this host", "reason", err)
				a.setEventsErr(err.Error())
			case err != nil:
				a.log.Error("runtime events stopped", "err", err)
				a.setEventsErr(err.Error())
			}
		})
	}
	if col.Cluster.Enabled {
		cluster, err := kube.InCluster("") // every namespace, every node
		if err != nil {
			a.log.Warn("cluster state is off", "reason", err)
		} else {
			c.Sources = append(c.Sources, collect.Source{Name: "cluster", Interval: col.Cluster.Interval.Std(), Collect: cluster.ClusterState})
			if *col.Cluster.Events {
				wg.Go(func() {
					a.shipClusterEvents(ctx, cluster, col.Cluster.Interval.Std(), func(batch []model.LogEntry) error {
						for i := range batch {
							maps.Copy(batch[i].Stream, col.Labels)
						}
						return emitLogs(batch)
					})
				})
			}
		}
	}
	if col.OTLP.Enabled {
		rcv := &otlp.Receiver{Listen: col.OTLP.Listen, Log: a.log, DropLabels: col.OTLP.Metrics.DropLabels,
			Metrics: func(batch []model.Sample) {
				for i := range batch {
					for k, v := range col.Labels {
						batch[i].Labels[k] = v
					}
				}
				emitSamples(batch)
			},
			Logs: func(batch []model.LogEntry) error {
				for i := range batch {
					stream := maps.Clone(batch[i].Stream) // shared between the entries of a request
					maps.Copy(stream, col.Labels)
					batch[i].Stream = stream
				}
				return emitLogs(batch)
			}}
		// Traces: on unless switched off. Spans travel as log entries, so
		// they share the labels, the queue and the limits of the logs.
		if t := col.OTLP.Traces; t.Enabled == nil || *t.Enabled {
			rcv.Traces = otlp.TraceOptions{Sample: 1, SlowerThan: t.SlowerThan.Std()}
			if t.Sample != nil {
				rcv.Traces.Sample = *t.Sample
			}
			if red.Active() {
				rcv.Redact = red.String
			}
			// Spans go on as the application sent them. They share the
			// byte allowance of the logs, so a chatty application cannot
			// send without bound.
			rcv.Spans = func(payload []byte) error {
				if !a.limits.traceBytes(len(payload)) {
					return nil // dropped and counted; telling the SDK to retry would only repeat it
				}
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				zw.Write(payload)
				zw.Close()
				return a.enqueue(a.traces, buf.Bytes())
			}
		}
		wg.Go(func() {
			if err := rcv.Run(ctx); err != nil {
				a.log.Error("OTLP endpoint stopped", "err", err)
			}
		})
	}
	c.Run(ctx)
	wg.Wait() // sources spool what they already read, probes detach
}

func (a *Agent) setEventsErr(msg string) {
	a.mu.Lock()
	a.eventsErr = msg
	a.mu.Unlock()
}

// pollConfig tells the server who this agent is (labels, facts) and what
// config it runs, and receives the config the server's profiles resolve to
// for it. A changed one is handed to Run. A config that does not validate
// is not applied; the reason is reported on the next poll and shown in the UI.
func (a *Agent) pollConfig(ctx context.Context, cache string, updates chan<- *config.Collection) {
	if !a.waitReady(ctx) {
		return
	}
	wait := time.Duration(0)
	failing := false
	// What runs on the host is looked at every few minutes and sent only
	// when it changed, or when the server says it has none.
	var (
		inv      []byte
		invTaken time.Time
		invSent  string
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		// Spread the polls of a large fleet instead of all arriving together.
		wait = a.PollEvery + time.Duration(rand.Int64N(int64(a.PollEvery)/5+1))
		a.mu.Lock()
		hash, lastErr, images := a.remoteHash, a.remoteErr, a.images
		if lastErr == "" {
			lastErr = a.eventsErr
		}
		a.mu.Unlock()

		body := map[string]any{
			"labels": a.cfg.LocalLabels, "facts": facts.Collect(ctx, a.cfg.AgentID, a.version),
			"config_hash": hash, "config_error": lastErr,
		}
		if inv == nil || time.Since(invTaken) > min(5*time.Minute, 10*a.PollEvery) {
			inv, _ = json.Marshal(inventory.Collect(ctx, images))
			invTaken = time.Now()
		}
		sending := string(inv)
		if sending != invSent {
			body["inventory"] = json.RawMessage(inv)
		}
		report, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.Server.URL, "/")+"/api/v1/agent/config", bytes.NewReader(report))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+a.cfg.Server.Token)
		req.Header.Set("X-Vigil-Agent", a.cfg.AgentID)
		if hash != "" {
			req.Header.Set("If-None-Match", `"`+hash+`"`)
		}
		resp, err := a.http.Do(req)
		if err != nil {
			if !failing && ctx.Err() == nil {
				a.log.Warn("cannot reach the server for managed config, keeping the current one", "err", err)
			}
			failing = true
			continue
		}
		failing = false
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode < 500 {
			invSent = sending
		}
		if resp.Header.Get("X-Vigil-Want-Inventory") != "" {
			invSent = "" // the server lost it, or never had it
		}

		var next *config.Collection
		newHash, newErr := hash, ""
		switch resp.StatusCode {
		case http.StatusNotModified:
			continue
		case http.StatusNoContent:
			if hash == "" {
				continue
			}
			os.Remove(cache)
			newHash = ""
		case http.StatusOK:
			c, err := a.parseManaged(string(respBody))
			if err != nil {
				a.log.Error("managed config rejected, keeping the current one", "err", err)
				newErr = err.Error()
				break
			}
			if err := os.WriteFile(cache+".tmp", respBody, 0o640); err == nil {
				os.Rename(cache+".tmp", cache)
			}
			next, newHash = c, config.Hash(string(respBody))
		default: // including 409: the server's profiles do not add up for this agent
			a.log.Warn("no managed config from the server, keeping the current one", "status", resp.StatusCode, "body", strings.TrimSpace(string(respBody)))
			continue
		}
		a.mu.Lock()
		a.remoteHash, a.remoteErr = newHash, newErr
		a.mu.Unlock()
		if newHash != hash {
			select {
			case updates <- next:
			case <-ctx.Done():
				return
			}
		}
		wait = 2 * time.Second // report the outcome promptly
	}
}

func (a *Agent) enqueue(s *shipper, data []byte) error {
	dropped, err := s.spool.Put(data)
	if err != nil {
		return fmt.Errorf("spool write: %w", err)
	}
	if dropped > 0 {
		a.limits.droppedSpool.Add(uint64(dropped))
		a.log.Warn("spool full, dropped oldest batches", "signal", s.name, "batches", dropped)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (a *Agent) sendLoop(ctx context.Context, s *shipper) {
	if !a.waitReady(ctx) {
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		id, data, ok, err := s.spool.Next()
		if err != nil {
			a.log.Error("spool read failed", "signal", s.name, "err", err)
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		retry, err := a.push(ctx, s, data)
		switch {
		case err == nil:
			backoff = time.Second
		case retry:
			a.log.Warn("push failed, will retry", "signal", s.name, "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		default:
			a.log.Error("server rejected batch, dropping it", "signal", s.name, "err", err)
		}
		if err := s.spool.Ack(id); err != nil {
			a.log.Error("spool ack failed", "signal", s.name, "err", err)
		}
	}
}

// push sends one encoded batch. retry is true for failures worth repeating
// (network errors, 5xx, 429); other 4xx mean the batch itself is bad.
func (a *Agent) push(ctx context.Context, s *shipper, data []byte) (retry bool, err error) {
	url := strings.TrimRight(a.cfg.Server.URL, "/") + s.path
	var extra map[string]string
	if s.split != nil {
		extra, data = s.split(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	for _, h := range []map[string]string{s.headers, extra} {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Encoding", s.encoding)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Server.Token)
	req.Header.Set("X-Vigil-Agent", a.cfg.AgentID)
	req.Header.Set("X-Vigil-Version", a.version)
	resp, err := a.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	if strings.Contains(string(msg), "HTTP request to an HTTPS server") {
		// The server was switched to TLS and this agent's file still says
		// http://. Nothing is wrong with the batch: keep it until the file is fixed.
		return true, fmt.Errorf("the server now expects https; change server.url in the agent's config (%w)", err)
	}
	return resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, err
}

// shipClusterEvents polls the cluster's events and ships the new ones as
// logs. How far it got is kept on disk, so a restart neither repeats events
// nor skips the ones that happened meanwhile; on the very first run it
// starts from now.
func (a *Agent) shipClusterEvents(ctx context.Context, cluster *kube.Client, every time.Duration, emit func([]model.LogEntry) error) {
	posFile := filepath.Join(a.cfg.SpoolDir, "cluster-events.pos")
	since := time.Now()
	if b, err := os.ReadFile(posFile); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
			since = t
		}
	}
	failing := false
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		entries, newest, err := cluster.Events(ctx, since)
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			a.log.Warn("cannot read cluster events, will keep trying", "err", err)
			failing = true
		case err == nil:
			failing = false
		}
		if len(entries) > 0 && emit(entries) == nil {
			since = newest
			os.WriteFile(posFile, []byte(since.Format(time.RFC3339Nano)), 0o640)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
