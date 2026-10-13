package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sync/atomic"
	"time"
)

// Traces are kept in VictoriaTraces. Agents send the spans they decided to
// keep, in OTLP form; the server hands them on, marked with the agent they
// came from. Reading goes through the server too: the trace store's Jaeger
// API (traces, services, operations, dependencies) and its LogsQL query
// over spans, both for signed-in users only.
//
// Without a trace store the spans are dropped here. The request and
// duration metrics agents count from every span do not depend on it.

const (
	signalTraces  = "traces"
	maxTraceBody  = 32 << 20 // compressed, as sent
	maxTracePlain = 128 << 20
)

var (
	traceIDRe   = regexp.MustCompile(`^[0-9a-fA-F]{16,32}$`)
	noStoreSaid atomic.Int64
)

func (s *Server) traceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/traces", s.ingest(s.bounded(s.handleTraces)))
	if s.traces == nil {
		none := s.viewer(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "this server has no trace store; set storage.traces_url to a VictoriaTraces instance", http.StatusNotImplemented)
		})
		mux.HandleFunc("GET /api/v1/traces/", none)
		mux.HandleFunc("POST /api/v1/traces/query", none)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(s.traces)
	to := func(path func(r *http.Request) (string, bool)) http.HandlerFunc {
		return s.asks(func(w http.ResponseWriter, r *http.Request) {
			p, ok := path(r)
			if !ok {
				http.NotFound(w, r)
				return
			}
			r.URL.Path, r.URL.RawPath = p, ""
			proxy.ServeHTTP(w, r)
		})
	}
	fixed := func(p string) func(*http.Request) (string, bool) {
		return func(*http.Request) (string, bool) { return p, true }
	}
	// The Jaeger query API, as tools that speak it expect it (Grafana's
	// Jaeger data source can point at <server>/api/v1/traces/jaeger).
	const jaeger = "/select/jaeger/api"
	mux.HandleFunc("GET /api/v1/traces/jaeger/api/services", to(fixed(jaeger+"/services")))
	mux.HandleFunc("GET /api/v1/traces/jaeger/api/services/{service}/operations", to(func(r *http.Request) (string, bool) {
		return jaeger + "/services/" + r.PathValue("service") + "/operations", true
	}))
	mux.HandleFunc("GET /api/v1/traces/jaeger/api/traces", to(fixed(jaeger+"/traces")))
	mux.HandleFunc("GET /api/v1/traces/jaeger/api/traces/{id}", to(func(r *http.Request) (string, bool) {
		id := r.PathValue("id")
		return jaeger + "/traces/" + id, traceIDRe.MatchString(id)
	}))
	mux.HandleFunc("GET /api/v1/traces/jaeger/api/dependencies", to(fixed(jaeger+"/dependencies")))
	// Spans as rows, filtered with LogsQL: what the trace lists use.
	mux.HandleFunc("GET /api/v1/traces/query", to(fixed("/select/logsql/query")))
	mux.HandleFunc("POST /api/v1/traces/query", to(fixed("/select/logsql/query")))
}

// handleTraces takes a gzip-compressed OTLP trace request from an agent
// and hands it to the trace store.
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	id, ok := agentID(w, r)
	if !ok {
		return
	}
	zr, err := gzip.NewReader(http.MaxBytesReader(w, r.Body, maxTraceBody))
	if err != nil {
		http.Error(w, "spans are sent gzip-compressed", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(zr, maxTracePlain+1))
	if err != nil || len(body) > maxTracePlain {
		http.Error(w, "unreadable or too large batch of spans", http.StatusBadRequest)
		return
	}
	// Spans count against the agent's allowance for logs, each in its own bucket.
	if rate := s.cfg.Limits.For(id).LogBytesPerSecond; rate > 0 && !s.limiter.allow(id, signalTraces, len(body), rate, time.Now()) {
		overLimit(w, "bytes of spans", rate)
		return
	}
	s.touch(id, r)
	if s.traces == nil {
		// Nowhere to keep them. Taking and dropping them keeps the agent's
		// queue from filling with something that will never be wanted.
		if last := noStoreSaid.Load(); time.Now().Unix()-last > 3600 && noStoreSaid.CompareAndSwap(last, time.Now().Unix()) {
			s.log.Warn("agents send spans, but this server has no trace store (storage.traces_url); they are dropped", "agent", id)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.traces.JoinPath("/insert/opentelemetry/v1/traces").String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	// Every span is marked with the agent it came through; the agent's id
	// has been checked against its token and holds no characters that
	// could break out of the header.
	req.Header.Set("VT-Extra-Fields", "agent="+id)
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("forward to trace store failed", "agent", id, "err", err)
		http.Error(w, "trace store unavailable", http.StatusServiceUnavailable) // agent retries
		return
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode/100 == 2:
		w.WriteHeader(http.StatusNoContent)
	case resp.StatusCode >= 500:
		s.log.Warn("trace store refused spans", "agent", id, "status", resp.StatusCode, "answer", string(msg))
		http.Error(w, "trace store unavailable", http.StatusServiceUnavailable)
	default:
		// The store understood and said no: sending the same again will not help.
		s.log.Warn("trace store rejected spans", "agent", id, "status", resp.StatusCode, "answer", string(msg))
		http.Error(w, "trace store rejected the spans: "+string(msg), http.StatusBadRequest)
	}
}
