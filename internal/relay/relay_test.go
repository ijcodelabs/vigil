package relay_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/relay"
	"github.com/ijcodelabs/vigil/internal/server"
)

// store stands in for both stores and counts what reaches them.
type store struct {
	mu     sync.Mutex
	writes int
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if strings.HasSuffix(r.URL.Path, "/api/v1/write") {
		s.writes++
	}
	s.mu.Unlock()
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusNoContent)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An agent that can only reach a relay enrols and sends through two relays
// in a row; the server sees the agent, with its own token, and the route.
func TestAgentThroughRelays(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &store{}
	backend := httptest.NewServer(st)
	defer backend.Close()

	scfg := &config.Server{DataDir: t.TempDir(), AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	scfg.Storage.MetricsURL, scfg.Storage.LogsURL = backend.URL, backend.URL
	srv, err := server.New(scfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	newRelay := func(id, upstream string) (*relay.Relay, *httptest.Server) {
		c := &config.Relay{RelayID: id, DataDir: t.TempDir(), MaxInFlight: 8}
		c.Server.URL = upstream
		r, err := relay.New(c, "test", log)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(r.Handler())
		t.Cleanup(ts.Close)
		return r, ts
	}
	_, site := newRelay("site", central.URL)
	_, rack := newRelay("rack-7", site.URL)

	admin := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, central.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-token-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	_, body := admin("POST", "/api/v1/fleet/enrollment", `{"name":"behind relays"}`)
	var made struct{ Token string }
	json.Unmarshal([]byte(body), &made)
	admin("PUT", "/api/v1/fleet/profiles/all", "config: {interval: 1s, labels: {managed: \"yes\"}}\n")

	on, off := true, false
	spool := t.TempDir()
	acfg := &config.Agent{AgentID: "web-01", SpoolDir: spool, SpoolMaxBytes: 1 << 20, RemoteConfig: &on}
	acfg.Interval, acfg.HostMetrics = config.Duration(100*time.Millisecond), &on
	acfg.Docker.Logs, acfg.Docker.Metrics = &off, &off
	acfg.Server.URL, acfg.Server.EnrollToken = rack.URL, made.Token
	a, err := agent.New(acfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	a.PollEvery = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	waitFor(t, "metrics through the relays", func() bool { st.mu.Lock(); defer st.mu.Unlock(); return st.writes > 0 })
	if tok, _ := os.ReadFile(filepath.Join(spool, "agent-token")); !strings.HasPrefix(string(tok), "vgl_") {
		t.Fatalf("the agent did not enrol through the relay: %q", tok)
	}
	// The server knows the agent, the route it took, and manages its config.
	var last string
	defer func() {
		if t.Failed() {
			t.Log(last)
		}
	}()
	waitFor(t, "the agent on the fleet list, in sync", func() bool {
		_, body := admin("GET", "/api/v1/agents", "")
		last = body
		return strings.Contains(body, `"id":"web-01"`) && strings.Contains(body, `"via":"rack-7,site"`) &&
			strings.Contains(body, `"config_state":"in sync"`) && strings.Contains(body, `"profiles":["all"]`)
	})

	// A relay passes on what agents send and nothing else.
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/", 404}, {"GET", "/api/v1/agents", 404}, {"GET", "/api/v1/query?query=up", 404},
		{"POST", "/api/v1/login", 404}, {"PUT", "/api/v1/fleet/profiles/x", 404}, {"GET", "/api/v1/write", 404},
		{"GET", "/healthz", 200}, {"GET", "/metrics", 200}, {"GET", "/api/v1/ca", 404},
	} {
		req, _ := http.NewRequest(c.method, rack.URL+c.path, nil)
		req.Header.Set("Authorization", "Bearer admin-token-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s through the relay: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	// The server's refusals come through as they are: a wrong token stays a 401.
	req, _ := http.NewRequest("POST", rack.URL+"/api/v1/logs", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer nope")
	req.Header.Set("X-Vigil-Agent", "web-01")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token through the relay: %v %v", resp, err)
	}

	// The server goes away: agents are told to try again, and what they
	// collected meanwhile arrives once it is back.
	central.Close()
	req, _ = http.NewRequest("POST", rack.URL+"/api/v1/logs", strings.NewReader(""))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("with the server down: %v %v", resp, err)
	}
	resp, _ := http.Get(rack.URL + "/metrics")
	text, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(text), `vigil_relay_info{relay="rack-7"`) || strings.Contains(string(text), "vigil_relay_requests_total 0\n") {
		t.Errorf("relay metrics:\n%s", text)
	}
}

// With its own authority a relay serves its certificate, not the server's.
func TestRelayOwnAuthority(t *testing.T) {
	c := &config.Relay{RelayID: "r", DataDir: t.TempDir(), MaxInFlight: 1}
	c.Server.URL = "http://127.0.0.1:1"
	c.TLS.SelfSigned = true
	r, err := relay.New(c, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(r.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/ca")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	onDisk, _ := os.ReadFile(filepath.Join(c.DataDir, "pki", "ca.pem"))
	if resp.StatusCode != 200 || len(b) == 0 || string(b) != string(onDisk) {
		t.Errorf("ca: %d, %d bytes", resp.StatusCode, len(b))
	}
}
