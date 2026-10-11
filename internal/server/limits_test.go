package server_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/logwire"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/server"
)

func TestServerSideLimits(t *testing.T) {
	store := &fakeStore{}
	backend := httptest.NewServer(store)
	defer backend.Close()
	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = backend.URL, backend.URL
	// Averaged over a minute: 600 samples and 6000 bytes of logs may come at once.
	cfg.Limits.SamplesPerSecond, cfg.Limits.LogBytesPerSecond = 10, 100
	cfg.Limits.Agents = map[string]config.IngestLimit{"big": {SamplesPerSecond: -1}}
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	send := func(agent, token, path string, body []byte) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("POST", central.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Vigil-Agent", agent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	samples := func(n int) []byte {
		out := make([]model.Sample, n)
		for i := range out {
			out[i] = model.Sample{Name: "m", Labels: map[string]string{"i": string(rune('a' + i%26))}, Value: 1, TS: time.Now().UnixMilli()}
		}
		return promwire.Encode(out)
	}
	if n, err := promwire.CountSamples(samples(123)); n != 123 || err != nil {
		t.Fatalf("CountSamples: %d %v", n, err)
	}

	// Within the allowance; then one batch that uses it up is still taken
	// whole; after that the agent is told to come back.
	if code, body, _ := send("web-01", "ingest", "/api/v1/write", samples(500)); code != 204 {
		t.Fatalf("first batch: %d %s", code, body)
	}
	if code, _, _ := send("web-01", "ingest", "/api/v1/write", samples(500)); code != 204 {
		t.Fatalf("the batch that crosses the limit: %d", code)
	}
	code, body, h := send("web-01", "ingest", "/api/v1/write", samples(1))
	if code != http.StatusTooManyRequests || h.Get("Retry-After") == "" || !strings.Contains(body, "10 samples per second") {
		t.Fatalf("over the limit: %d %q %v", code, body, h)
	}
	store.mu.Lock()
	stored := len(store.samples)
	store.mu.Unlock()
	if stored != 1000 {
		t.Errorf("the store got %d samples, want the 1000 that were accepted", stored)
	}
	// Another agent is not affected, and one with its own setting has no limit.
	if code, _, _ := send("web-02", "ingest", "/api/v1/write", samples(100)); code != 204 {
		t.Errorf("another agent: %d", code)
	}
	for i := 0; i < 3; i++ {
		if code, _, _ := send("big", "ingest", "/api/v1/write", samples(2000)); code != 204 {
			t.Errorf("agent without a limit: %d", code)
		}
	}
	// Garbage is refused as garbage, not counted.
	if code, _, _ := send("web-03", "ingest", "/api/v1/write", []byte("not a write request")); code != http.StatusBadRequest {
		t.Errorf("malformed write: %d", code)
	}

	// Logs have their own allowance; being held back on metrics does not stop them.
	lines := func(n, size int) []byte {
		out := make([]model.LogEntry, n)
		for i := range out {
			out[i] = model.LogEntry{Time: time.Now().UnixNano(), Msg: strings.Repeat("x", size), Stream: map[string]string{"source": "test"}}
		}
		return logwire.Encode(out)
	}
	if code, body, _ := send("web-01", "ingest", "/api/v1/logs", lines(10, 700)); code != 204 {
		t.Fatalf("logs within the limit: %d %s", code, body)
	}
	if code, body, _ := send("web-01", "ingest", "/api/v1/logs", lines(1, 10)); code != http.StatusTooManyRequests || !strings.Contains(body, "bytes of logs") {
		t.Fatalf("logs over the limit: %d %s", code, body)
	}

	// The fleet list says who is being held back, on what.
	req, _ := http.NewRequest("GET", central.URL+"/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer admin-token-secret")
	resp, _ := http.DefaultClient.Do(req)
	list, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(list), `"limited":["metrics","logs"]`) || strings.Count(string(list), `"limited"`) != 1 {
		t.Errorf("agents: %s", list)
	}

	// A revoked token stops working at once, although tokens are remembered.
	admin := func(method, path string) string {
		req, _ := http.NewRequest(method, central.URL+path, nil)
		req.Header.Set("Authorization", "Bearer admin-token-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	issued := admin("POST", "/api/v1/agents/db-01/token")
	tok := issued[strings.Index(issued, "vgl_"):]
	tok = tok[:strings.IndexByte(tok, '"')]
	for i := 0; i < 2; i++ {
		if code, _, _ := send("db-01", tok, "/api/v1/write", samples(1)); code != 204 {
			t.Fatalf("own token: %d", code)
		}
	}
	admin("DELETE", "/api/v1/agents/db-01/token")
	if code, _, _ := send("db-01", tok, "/api/v1/write", samples(1)); code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", code)
	}
}
