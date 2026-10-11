// Package relay passes what agents send on to the server. A relay holds no
// state and decides nothing: the server still checks every agent's token,
// so a relay need not be trusted with more than the traffic it carries.
// When the server cannot be reached agents are told to try again, and keep
// their data in their own spools.
package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
)

// forwarded are the only requests a relay passes on: what agents send.
// The interface and the rest of the API are not reachable through a relay.
var forwarded = map[string]bool{
	"/api/v1/write": true, "/api/v1/logs": true, "/api/v1/events": true, "/api/v1/captures": true, "/api/v1/traces": true,
	"/api/v1/agent/config": true, "/api/v1/enroll": true,
}

const maxBody = 128 << 20

type Relay struct {
	cfg     *config.Relay
	log     *slog.Logger
	version string
	ca      *pki.Authority // nil unless tls.self_signed
	target  *url.URL
	slots   chan struct{}

	mu    sync.Mutex
	proxy *httputil.ReverseProxy // nil until the server's certificate could be checked

	requests, failures, busy atomic.Uint64
	lastOK                   atomic.Int64
}

func New(cfg *config.Relay, version string, log *slog.Logger) (*Relay, error) {
	target, err := url.Parse(cfg.Server.URL)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("server.url %q is not a URL", cfg.Server.URL)
	}
	r := &Relay{cfg: cfg, log: log, version: version, target: target, slots: make(chan struct{}, cfg.MaxInFlight)}
	if cfg.TLS.SelfSigned {
		if r.ca, err = pki.Open(filepath.Join(cfg.DataDir, "pki"), cfg.TLS.Names); err != nil {
			return nil, fmt.Errorf("certificate authority: %w", err)
		}
	}
	return r, nil
}

// upstream returns the proxy to the server, building it on first use: with
// a pinned authority that needs the server to be reachable once.
func (r *Relay) upstream() (*httputil.ReverseProxy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proxy != nil {
		return r.proxy, nil
	}
	tc, err := pki.ClientTLS(pki.ClientOptions{ServerURL: r.cfg.Server.URL, CAFile: r.cfg.Server.CAFile, CAPin: r.cfg.Server.CAPin,
		CacheFile: filepath.Join(r.cfg.DataDir, "upstream-ca.pem"), Insecure: r.cfg.Server.TLSInsecure})
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	tr.MaxIdleConnsPerHost = 32 // many agents share few connections to the server
	tr.ResponseHeaderTimeout = 60 * time.Second
	r.proxy = &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(r.target)
			p.Out.Host = r.target.Host
			// Through several relays the nearest to the agent comes first.
			via := r.cfg.RelayID
			if prev := p.In.Header.Get("X-Vigil-Relay"); prev != "" {
				via = prev + "," + via
			}
			p.Out.Header.Set("X-Vigil-Relay", via)
			p.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode < 500 {
				r.lastOK.Store(time.Now().Unix())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			r.failures.Add(1)
			if !errors.Is(err, context.Canceled) {
				r.log.Warn("cannot pass a request on to the server", "path", req.URL.Path, "err", err)
			}
			// 503 makes the agent keep the batch and try again.
			http.Error(w, "relay "+r.cfg.RelayID+" cannot reach the server", http.StatusServiceUnavailable)
		},
		ErrorLog: slog.NewLogLogger(r.log.Handler(), slog.LevelDebug),
	}
	return r.proxy, nil
}

func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	// The relay's own authority, for agents that pin it; never the server's.
	mux.HandleFunc("GET /api/v1/ca", func(w http.ResponseWriter, _ *http.Request) {
		if r.ca == nil {
			http.Error(w, "this relay does not use its own certificate authority", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(r.ca.CAPEM())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		age := -1.0
		if t := r.lastOK.Load(); t > 0 {
			age = time.Since(time.Unix(t, 0)).Seconds()
		}
		fmt.Fprintf(w, "# TYPE vigil_relay_requests_total counter\nvigil_relay_requests_total %d\n"+
			"# TYPE vigil_relay_failures_total counter\nvigil_relay_failures_total %d\n"+
			"# TYPE vigil_relay_rejected_busy_total counter\nvigil_relay_rejected_busy_total %d\n"+
			"# TYPE vigil_relay_in_flight gauge\nvigil_relay_in_flight %d\n"+
			"# HELP vigil_relay_last_success_age_seconds Seconds since the server last answered; -1 if it never has.\n"+
			"# TYPE vigil_relay_last_success_age_seconds gauge\nvigil_relay_last_success_age_seconds %.0f\n"+
			"# TYPE vigil_relay_info gauge\nvigil_relay_info{relay=%q,version=%q} 1\n",
			r.requests.Load(), r.failures.Load(), r.busy.Load(), len(r.slots), age, r.cfg.RelayID, r.version)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || !forwarded[req.URL.Path] {
			http.Error(w, "this is a vigil relay: it only passes on what agents send. Open the server itself for the interface and the API.", http.StatusNotFound)
			return
		}
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		default:
			r.busy.Add(1)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "relay is busy, try again", http.StatusServiceUnavailable)
			return
		}
		r.requests.Add(1)
		p, err := r.upstream()
		if err != nil {
			r.failures.Add(1)
			r.log.Warn("cannot set up the connection to the server", "err", err)
			http.Error(w, "relay "+r.cfg.RelayID+" cannot reach the server", http.StatusServiceUnavailable)
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, maxBody)
		p.ServeHTTP(w, req)
	})
	return mux
}

// Run serves until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	srv := &http.Server{Addr: r.cfg.Listen, Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: slog.NewLogLogger(r.log.Handler(), slog.LevelDebug)}
	errc := make(chan error, 1)
	switch {
	case r.ca != nil:
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.ca.GetCertificate}
		go func() { errc <- srv.ListenAndServeTLS("", "") }()
		r.log.Info("relay listening, encrypted with its own certificate authority", "addr", r.cfg.Listen, "relay", r.cfg.RelayID,
			"server", r.cfg.Server.URL, "ca_pin", r.ca.Pin())
	case r.cfg.TLS.CertFile != "":
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		go func() { errc <- srv.ListenAndServeTLS(r.cfg.TLS.CertFile, r.cfg.TLS.KeyFile) }()
		r.log.Info("relay listening, encrypted", "addr", r.cfg.Listen, "relay", r.cfg.RelayID, "server", r.cfg.Server.URL)
	default:
		go func() { errc <- srv.ListenAndServe() }()
		r.log.Warn("relay listening WITHOUT encryption: agents' tokens and data reach it in the clear; set tls in its config",
			"addr", r.cfg.Listen, "relay", r.cfg.RelayID, "server", r.cfg.Server.URL)
	}
	if !strings.HasPrefix(r.cfg.Server.URL, "https://") {
		r.log.Warn("the connection from this relay to the server is not encrypted", "server", r.cfg.Server.URL)
	}
	// Find out early whether the server can be reached; agents are served either way.
	go func() {
		if _, err := r.upstream(); err != nil {
			r.log.Warn("cannot check the server's certificate yet, will try again when agents send", "err", err)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}
