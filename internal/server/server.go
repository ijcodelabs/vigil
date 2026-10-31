// Package server is the central node: it authenticates agents, forwards
// their data to storage, tracks heartbeats and fronts the query API.
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ijcodelabs/vigil/internal/ai"
	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/collect"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/discovery"
	"github.com/ijcodelabs/vigil/internal/incidents"
	"github.com/ijcodelabs/vigil/internal/kube"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/pki"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/rules"
	"github.com/ijcodelabs/vigil/internal/ui"
)

const (
	maxWriteBody  = 32 << 20
	selfAgentName = "vigil-server"
)

var labelNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,200}$`)

// hardened puts the headers on every answer that tell a browser to be
// careful with it, whatever produced the answer.
func (s *Server) hardened(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.viaTLS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/alertmanager/") || strings.HasPrefix(r.URL.Path, "/select/") {
			h.Set("Cache-Control", "no-store") // answers hold data, and sometimes secrets shown once
		}
		next.ServeHTTP(w, r)
	})
}

var agentIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type Server struct {
	cfg     *config.Server
	log     *slog.Logger
	store   *Store
	metrics *url.URL
	logs    *url.URL
	traces  *url.URL // nil: no trace store
	vault   vault    // the secret store set up in the interface (secrets.go)
	am      *alerting.Alertmanager
	rules   *rules.Manager
	runCtx  context.Context // lifetime of the rule loops
	logins  loginLimiter
	started time.Time
	proxies []*net.IPNet // trusted_proxies, parsed
	limiter ingestLimiter
	slots   chan struct{} // nil: no bound on batches in flight
	busy    atomic.Uint64
	tokenMu sync.Mutex
	tokens  map[string]string // token hash -> agent id
	touchMu sync.Mutex
	touched map[string]touchState
	// rolloutMu keeps a profile save, a rollout action and the periodic
	// step from interleaving: each leaves profiles and rollouts consistent.
	rolloutMu sync.Mutex
	fleet     fleetState
	// ca is the server's own certificate authority; nil unless tls.self_signed.
	ca *pki.Authority
	// rulesStarted is set once rule evaluation runs; before that, changes
	// to the integrations in use need not restart it.
	rulesStarted atomic.Bool
	http         *http.Client
	// incidents is what the Home page reads (briefing.go); incidentState
	// is how its last update went.
	incidents     *incidents.Tracker
	incidentState atomic.Pointer[incidentSync]
	comingState   atomic.Pointer[comingState] // forecasts.go
	spendState    atomic.Pointer[Spend]
	// ai explains incidents (explain.go); nil when no provider is set up.
	ai ai.Provider
}

func New(cfg *config.Server, log *slog.Logger) (*Server, error) {
	u, err := url.Parse(cfg.Storage.MetricsURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage.metrics_url %q is not a URL", cfg.Storage.MetricsURL)
	}
	var tu *url.URL
	if cfg.Storage.TracesURL != "" {
		if tu, err = url.Parse(cfg.Storage.TracesURL); err != nil || tu.Host == "" {
			return nil, fmt.Errorf("storage.traces_url %q is not a URL", cfg.Storage.TracesURL)
		}
	}
	lu, err := url.Parse(cfg.Storage.LogsURL)
	if err != nil || lu.Host == "" {
		return nil, fmt.Errorf("storage.logs_url %q is not a URL", cfg.Storage.LogsURL)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	st, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	tracker, err := incidents.New(st.db)
	if err == nil {
		err = initExplanations(st.db)
	}
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Server{cfg: cfg, log: log, store: st, metrics: u, logs: lu, traces: tu, runCtx: context.Background(), incidents: tracker,
		logins:  loginLimiter{fails: map[string][]time.Time{}},
		started: time.Now(),
		limiter: ingestLimiter{buckets: map[string]*bucket{}},
		tokens:  map[string]string{}, touched: map[string]touchState{},
		http: &http.Client{Timeout: 30 * time.Second}}
	s.openVault()
	if a := cfg.AI; a.Provider == "anthropic" {
		key := ai.FileKey(a.APIKeyFile)
		if a.APIKeyFile == "" {
			key = func(ctx context.Context) (string, error) { return s.secret(ctx, "ai_api_key") }
		}
		s.ai = ai.NewAnthropic(a.Model, a.Effort, key, a.BaseURL)
	}
	for _, t := range cfg.TrustedProxies {
		if !strings.Contains(t, "/") {
			if ip := net.ParseIP(t); ip != nil && ip.To4() != nil {
				t += "/32"
			} else {
				t += "/128"
			}
		}
		if _, n, err := net.ParseCIDR(t); err == nil {
			s.proxies = append(s.proxies, n)
		}
	}
	if cfg.Limits.MaxInFlight > 0 {
		s.slots = make(chan struct{}, cfg.Limits.MaxInFlight)
	}
	if cfg.TLS.SelfSigned {
		names := append([]string{}, cfg.TLS.Names...)
		if ext, err := url.Parse(cfg.ExternalURL); err == nil && ext.Hostname() != "" {
			names = append(names, ext.Hostname())
		}
		if s.ca, err = pki.Open(filepath.Join(cfg.DataDir, "pki"), names); err != nil {
			st.Close()
			return nil, fmt.Errorf("certificate authority: %w", err)
		}
	}
	if err := s.startAlerting(); err != nil {
		st.Close()
		return nil, err
	}
	if err := s.loadFleet(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.syncIntegrations(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.syncProvided(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() error {
	s.rules.Stop()
	s.am.Close()
	return s.store.Close()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /api/v1/write", s.ingest(s.bounded(s.handleWrite)))
	s.authRoutes(mux)
	s.fleetRoutes(mux)
	s.briefingRoutes(mux)
	s.explainRoutes(mux)
	s.logLineRoutes(mux)
	s.secretRoutes(mux)

	// Read-only Prometheus query API, served by the metrics store.
	proxy := httputil.NewSingleHostReverseProxy(s.metrics)
	query := s.asks(proxy.ServeHTTP)
	// The last two are asked by tools that speak Prometheus' API (Grafana's
	// data source, on connecting and for the help text of metrics).
	for _, p := range []string{"query", "query_range", "series", "labels", "metadata", "status/buildinfo"} {
		mux.HandleFunc("GET /api/v1/"+p, query)
		mux.HandleFunc("POST /api/v1/"+p, query)
	}
	// The label name becomes part of the address asked of the store, so it
	// has to be a label name and nothing else.
	labelValues := func(w http.ResponseWriter, r *http.Request) {
		if !labelNameRe.MatchString(r.PathValue("name")) {
			http.Error(w, "not a label name", http.StatusBadRequest)
			return
		}
		query(w, r)
	}
	mux.HandleFunc("GET /api/v1/label/{name}/values", labelValues)
	mux.HandleFunc("POST /api/v1/label/{name}/values", labelValues)

	// Read-only log query API (LogsQL), served by the log store.
	logProxy := httputil.NewSingleHostReverseProxy(s.logs)
	logProxy.FlushInterval = -1 // live tail is a long streaming response
	logQuery := s.asks(func(w http.ResponseWriter, r *http.Request) {
		op := r.PathValue("op")
		if !logQueryOps[op] {
			http.NotFound(w, r)
			return
		}
		r.URL.Path, r.URL.RawPath = "/select/logsql/"+op, ""
		logProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/v1/logs/{op}", logQuery)
	mux.HandleFunc("POST /api/v1/logs/{op}", logQuery)
	// The same under the log store's own paths, for tools that expect to
	// talk to a VictoriaLogs (its Grafana data source) and are given this
	// server's address instead.
	mux.HandleFunc("GET /select/logsql/{op}", logQuery)
	mux.HandleFunc("POST /select/logsql/{op}", logQuery)
	mux.HandleFunc("POST /api/v1/logs", s.ingest(s.bounded(s.handleLogs)))
	s.traceRoutes(mux)
	s.alertingRoutes(mux)
	s.securityRoutes(mux)
	s.enrollRoutes(mux)
	s.dashboardRoutes(mux)
	s.queryRoutes(mux)
	s.sloRoutes(mux)
	s.sloReportRoutes(mux)
	s.sloTemplateRoutes(mux)
	s.tokenRoutes(mux)
	mux.Handle("GET /docs/", ui.DocsHandler())
	mux.Handle("/", ui.Handler())
	return s.hardened(mux)
}

var logQueryOps = map[string]bool{"query": true, "tail": true, "hits": true, "stats_query": true,
	"stats_query_range": true, "streams": true, "stream_field_names": true, "stream_field_values": true,
	"field_names": true, "field_values": true}

// agentID validates the caller's X-Vigil-Agent header, answering 400 itself.
func agentID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.Header.Get("X-Vigil-Agent")
	if !agentIDRe.MatchString(id) || id == selfAgentName {
		http.Error(w, "missing or invalid X-Vigil-Agent header", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

type touchState struct {
	at                 time.Time
	version, addr, via string
}

// touch records that an agent was heard from. Agents send several batches
// every few seconds; the database hears about it at most every five.
func (s *Server) touch(id string, r *http.Request) {
	addr := s.clientAddr(r)
	// The relay header is for display only: an agent can lie in it about
	// nothing but its own route.
	via := r.Header.Get("X-Vigil-Relay")
	if len(via) > 200 || strings.ContainsAny(via, "<>\"'\n") {
		via = ""
	}
	now := time.Now()
	st := touchState{now, r.Header.Get("X-Vigil-Version"), addr, via}
	s.touchMu.Lock()
	prev, seen := s.touched[id]
	fresh := seen && now.Sub(prev.at) < 5*time.Second && prev.version == st.version && prev.addr == st.addr && prev.via == st.via
	if !fresh {
		s.touched[id] = st
	}
	s.touchMu.Unlock()
	if fresh {
		return
	}
	if err := s.store.Touch(id, st.version, addr, via, now); err != nil {
		s.log.Error("heartbeat update failed", "agent", id, "err", err)
	}
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	id, ok := agentID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWriteBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if rate := s.cfg.Limits.For(id).SamplesPerSecond; rate > 0 {
		n, err := promwire.CountSamples(body)
		if err != nil {
			http.Error(w, "bad write request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !s.limiter.allow(id, signalMetrics, n, rate, time.Now()) {
			overLimit(w, "samples", rate)
			return
		}
	}
	if err := s.forward(r.Context(), id, body); err != nil {
		s.log.Warn("forward to metrics store failed", "agent", id, "err", err)
		http.Error(w, "metrics store unavailable", http.StatusServiceUnavailable) // agent retries
		return
	}
	s.touch(id, r)
	w.WriteHeader(http.StatusNoContent)
}

// forward writes an encoded batch to the metrics store. The store stamps
// every series with agent=<id>, so an agent cannot write under another name.
// An empty agentID writes the series as they are; that is for rule results,
// whose own agent label says which host they are about.
func (s *Server) forward(ctx context.Context, agentID string, body []byte) error {
	u := s.metrics.JoinPath("/api/v1/write")
	if agentID != "" {
		u.RawQuery = url.Values{"extra_label": {"agent=" + agentID}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// Run serves HTTP and the background loops until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.waitForStore(ctx, 30*time.Second)
	go s.heartbeatLoop(ctx)
	go s.priceLoop(ctx)
	go s.incidentLoop(ctx)
	go s.forecastLoop(ctx)
	go s.sloReportLoop(ctx)
	go s.spendLoop(ctx)
	go s.vaultLoop(ctx)
	go func() { // rollouts move on by themselves
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.rolloutTick(now)
			}
		}
	}()
	if err := s.loadRules(ctx); err != nil {
		return err
	}
	var pods *kube.Client
	var podsErr error
	if s.cfg.Scrape.Kubernetes.Enabled {
		pods, podsErr = kube.InCluster(s.cfg.Scrape.Kubernetes.Node)
	}
	if ds := discovery.FromConfig(s.cfg.Scrape, pods, podsErr); len(ds) > 0 {
		c := &collect.Collector{
			Interval:    s.cfg.Interval.Std(),
			Discoverers: ds,
			Log:         s.log,
			Emit: func(b []model.Sample) {
				if err := s.forward(ctx, selfAgentName, promwire.Encode(b)); err != nil {
					s.log.Warn("writing scraped samples failed", "err", err)
				}
			},
		}
		go c.Run(ctx)
	}

	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: stdlog.New(connErrors{s.log}, "", 0)}
	errc := make(chan error, 1)
	switch {
	case s.ca != nil:
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.ca.GetCertificate}
		go func() { errc <- srv.ListenAndServeTLS("", "") }()
		s.log.Info("server listening, encrypted with its own certificate authority", "addr", s.cfg.Listen, "ca_pin", s.ca.Pin())
	case s.cfg.TLS.CertFile != "":
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		go func() { errc <- srv.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile) }()
		s.log.Info("server listening, encrypted", "addr", s.cfg.Listen)
	default:
		go func() { errc <- srv.ListenAndServe() }()
		s.log.Warn("server listening WITHOUT encryption: tokens and data cross the network in the clear; set tls in the config, or put a TLS proxy in front",
			"addr", s.cfg.Listen)
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}

// heartbeatLoop publishes vigil_agent_up{agent=...} so "host down" can be
// alerted on like any other metric. A pushing agent has no `up` of its own.
func (s *Server) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.writeHeartbeats(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) writeHeartbeats(ctx context.Context) {
	now := time.Now()
	agents, err := s.store.Agents(now, s.cfg.HeartbeatTimeout.Std())
	if err != nil {
		s.log.Error("listing agents failed", "err", err)
		return
	}
	if len(agents) == 0 {
		return
	}
	// One request per agent: the store's extra_label sets `agent` for us.
	for _, a := range agents {
		v := 0.0
		if a.Up {
			v = 1
		}
		b := []model.Sample{
			{Name: "vigil_agent_up", Labels: map[string]string{}, Value: v, TS: now.UnixMilli()},
			{Name: "vigil_agent_last_seen_timestamp_seconds", Labels: map[string]string{}, Value: float64(a.LastSeen.Unix()), TS: now.UnixMilli()},
		}
		// Only for agents the server has ever held back, so a quiet fleet
		// costs no series.
		for _, signal := range []string{signalMetrics, signalLogs} {
			if n, _ := s.limiter.state(a.ID, signal, now); n > 0 {
				b = append(b, model.Sample{Name: "vigil_agent_limited_batches_total", Labels: map[string]string{"signal": signal},
					Value: float64(n), TS: now.UnixMilli()})
			}
		}
		if err := s.forward(ctx, a.ID, promwire.Encode(b)); err != nil {
			s.log.Warn("writing heartbeat failed", "agent", a.ID, "err", err)
			return
		}
	}
}

// waitForStore gives the metric store a moment to come up. Started together
// (one compose file, one boot) the server is usually first, and would
// otherwise greet every start with a round of failed rule evaluations.
func (s *Server) waitForStore(ctx context.Context, limit time.Duration) {
	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(limit)
	for {
		resp, err := hc.Get(s.metrics.JoinPath("/health").String())
		if err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			s.log.Warn("the metric store does not answer yet; starting anyway", "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// connErrors receives what net/http says about broken connections. Failed
// TLS handshakes are mostly scanners and clients still using plain HTTP, one
// line per attempt, so they are kept for debugging only.
type connErrors struct{ log *slog.Logger }

func (c connErrors) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if strings.Contains(msg, "TLS handshake error") {
		c.log.Debug(msg)
	} else {
		c.log.Warn(msg)
	}
	return len(p), nil
}
