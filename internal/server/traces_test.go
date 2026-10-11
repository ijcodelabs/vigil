package server_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// Spans from an agent reach the trace store unpacked and marked with the
// agent; reading goes through the server, for signed-in users, and only to
// the store's query paths.
func TestTraceStore(t *testing.T) {
	var mu sync.Mutex
	var got []string
	down := false
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" extra="+r.Header.Get("VT-Extra-Fields")+" auth="+r.Header.Get("Authorization")+r.Header.Get("Cookie")+" body="+string(body))
		io.WriteString(w, `{"data":["frontend"]}`)
	}))
	defer store.Close()

	start := func(tracesURL string) (*httptest.Server, func()) {
		cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
		cfg.Storage.MetricsURL, cfg.Storage.LogsURL, cfg.Storage.TracesURL = "http://127.0.0.1:1", "http://127.0.0.1:1", tracesURL
		srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv.Handler())
		return ts, func() { ts.Close(); srv.Close() }
	}
	central, stop := start(store.URL)
	defer stop()
	call := func(base, method, path, token, agent string, body []byte) (int, string) {
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if agent != "" {
			req.Header.Set("X-Vigil-Agent", agent)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(got) == 0 {
			return ""
		}
		return got[len(got)-1]
	}

	if code, body := call(central.URL, "POST", "/api/v1/traces", "ingest", "web-01", gz([]byte("otlp-bytes"))); code != http.StatusNoContent {
		t.Fatalf("send spans: %d %s", code, body)
	}
	if l := last(); !strings.HasPrefix(l, "POST /insert/opentelemetry/v1/traces?") || !strings.Contains(l, "extra=agent=web-01") || !strings.HasSuffix(l, "body=otlp-bytes") || !strings.Contains(l, "auth= ") {
		t.Errorf("what the store got: %s", l)
	}
	// Not an agent, not compressed, no agent name: refused before the store.
	n := len(got)
	for what, c := range map[string][3]string{"admin token": {"admin-token-secret", "web-01", "401"}, "no token": {"", "web-01", "401"}, "no agent": {"ingest", "", "400"}} {
		if code, _ := call(central.URL, "POST", "/api/v1/traces", c[0], c[1], gz([]byte("x"))); itoa(code) != c[2] {
			t.Errorf("%s: %d, want %s", what, code, c[2])
		}
	}
	if code, _ := call(central.URL, "POST", "/api/v1/traces", "ingest", "web-01", []byte("not gzip")); code != http.StatusBadRequest {
		t.Errorf("uncompressed spans: %d", code)
	}
	if len(got) != n {
		t.Errorf("refused requests reached the store: %v", got[n:])
	}
	// The store is away: the agent is told to come back.
	mu.Lock()
	down = true
	mu.Unlock()
	if code, _ := call(central.URL, "POST", "/api/v1/traces", "ingest", "web-01", gz([]byte("x"))); code != http.StatusServiceUnavailable {
		t.Errorf("with the store down: %d", code)
	}
	mu.Lock()
	down = false
	mu.Unlock()

	// Reading: signed in only, and only where queries live.
	for path, want := range map[string]string{
		"/api/v1/traces/jaeger/api/services":                                "GET /select/jaeger/api/services?",
		"/api/v1/traces/jaeger/api/services/frontend/operations":            "GET /select/jaeger/api/services/frontend/operations?",
		"/api/v1/traces/jaeger/api/traces?service=frontend&limit=5":         "GET /select/jaeger/api/traces?service=frontend&limit=5",
		"/api/v1/traces/jaeger/api/traces/4bf92f3577b34da6a3ce929d0e0e4736": "GET /select/jaeger/api/traces/4bf92f3577b34da6a3ce929d0e0e4736?",
		"/api/v1/traces/jaeger/api/dependencies?lookback=1":                 "GET /select/jaeger/api/dependencies?lookback=1",
		"/api/v1/traces/query?query=kind:2":                                 "GET /select/logsql/query?query=kind:2",
	} {
		if code, _ := call(central.URL, "GET", path, "", "", nil); code != http.StatusUnauthorized {
			t.Errorf("%s without signing in: %d", path, code)
		}
		if code, body := call(central.URL, "GET", path, "admin-token-secret", "", nil); code != 200 || !strings.Contains(body, "frontend") {
			t.Errorf("%s: %d %s", path, code, body)
		}
		if l := last(); !strings.HasPrefix(l, want) || !strings.Contains(l, "auth= ") {
			t.Errorf("%s reached the store as: %s", path, l)
		}
	}
	n = len(got)
	for _, path := range []string{"/api/v1/traces/jaeger/api/traces/..%2f..%2f..%2finsert", "/api/v1/traces/jaeger/api/traces/not-an-id",
		"/api/v1/traces/insert/opentelemetry/v1/traces", "/api/v1/traces/delete", "/api/v1/traces/jaeger/api/unknown"} {
		if code, _ := call(central.URL, "GET", path, "admin-token-secret", "", nil); code != 404 {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
	if len(got) != n {
		t.Errorf("a request outside the query paths reached the store: %v", got[n:])
	}

	// A server without a trace store takes spans and drops them, and says so to readers.
	bare, stopBare := start("")
	defer stopBare()
	if code, _ := call(bare.URL, "POST", "/api/v1/traces", "ingest", "web-01", gz([]byte("x"))); code != http.StatusNoContent {
		t.Errorf("spans to a server without a trace store: %d", code)
	}
	if code, body := call(bare.URL, "GET", "/api/v1/traces/jaeger/api/services", "admin-token-secret", "", nil); code != http.StatusNotImplemented || !strings.Contains(body, "traces_url") {
		t.Errorf("reading without a trace store: %d %s", code, body)
	}
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune { return r }, string(rune('0'+n/100))+string(rune('0'+n/10%10))+string(rune('0'+n%10))))
}
