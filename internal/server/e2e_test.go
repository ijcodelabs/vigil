package server_test

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
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/server"
)

// fakeStore stands in for VictoriaMetrics: it records remote writes and
// applies extra_label the way the real store does.
type fakeStore struct {
	mu           sync.Mutex
	samples      []model.Sample
	logs         []map[string]string
	streamFields string
	down         bool
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/v1/query" {
		io.WriteString(w, `{"status":"success","auth":"`+r.Header.Get("Authorization")+`"}`)
		return
	}
	if r.URL.Path == "/select/logsql/query" {
		io.WriteString(w, "logsql:"+r.URL.Query().Get("query")+":"+r.Header.Get("Authorization"))
		return
	}
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/insert/jsonline" {
		f.streamFields = r.URL.Query().Get("_stream_fields")
		dec := json.NewDecoder(r.Body)
		for {
			var row map[string]string
			if dec.Decode(&row) != nil {
				break
			}
			f.logs = append(f.logs, row)
		}
		return
	}
	body, _ := io.ReadAll(r.Body)
	ss, err := promwire.Decode(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	k, v, _ := strings.Cut(r.URL.Query().Get("extra_label"), "=")
	for _, s := range ss {
		s.Labels[k] = v
		f.samples = append(f.samples, s)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeStore) find(name, agent string) (model.Sample, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.samples {
		if s.Name == name && s.Labels["agent"] == agent {
			return s, true
		}
	}
	return model.Sample{}, false
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

func TestAgentToStore(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeStore{down: true} // start with storage unavailable
	vm := httptest.NewServer(store)
	defer vm.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "demo_requests_total{job=\"theirs\",code=\"200\",agent=\"agt-7\"} 7\n")
	}))
	defer target.Close()

	scfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin",
		HeartbeatTimeout: config.Duration(time.Minute)}
	scfg.Storage.MetricsURL, scfg.Storage.LogsURL = vm.URL, vm.URL
	srv, err := server.New(scfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	on, off := true, false
	acfg := &config.Agent{AgentID: "web-01", SpoolDir: t.TempDir(), SpoolMaxBytes: 1 << 20, RemoteConfig: &off}
	acfg.Interval, acfg.HostMetrics, acfg.Labels = config.Duration(100*time.Millisecond), &on, map[string]string{"env": "test"}
	acfg.Docker.Logs, acfg.Docker.Metrics = &on, &on
	acfg.Server.URL, acfg.Server.Token = central.URL, "ingest"
	acfg.Scrape.Static = []config.StaticJob{
		{Job: "demo", Targets: []string{strings.TrimPrefix(target.URL, "http://")}},
		{Job: "dead", Targets: []string{"127.0.0.1:1"}},
	}
	logFile := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(logFile, nil, 0o644)
	acfg.Logs.Templates = map[string]config.LogTemplate{"kv": {Parse: "logfmt", Labels: []string{"level"}}}
	acfg.Logs.Files = []config.LogFiles{{Paths: []string{logFile}, Template: "kv", Labels: map[string]string{"app": "demo"}}}
	a, err := agent.New(acfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// Batches collected while storage is down must arrive once it is back.
	time.Sleep(400 * time.Millisecond)
	// "agent" and "_time" below are an attempt to spoof server-owned fields.
	f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("level=error msg=\"payment failed\" order=42 agent=someone-else _time=1999\n")
	f.Close()
	time.Sleep(600 * time.Millisecond)
	if _, ok := store.find("demo_requests_total", "web-01"); ok {
		t.Fatal("sample stored while storage was down")
	}
	store.mu.Lock()
	store.down = false
	store.mu.Unlock()

	waitFor(t, "scraped sample", func() bool { _, ok := store.find("demo_requests_total", "web-01"); return ok })
	s, _ := store.find("demo_requests_total", "web-01")
	if s.Value != 7 || s.Labels["job"] != "demo" || s.Labels["exported_job"] != "theirs" ||
		s.Labels["env"] != "test" || s.Labels["code"] != "200" || s.Labels["instance"] == "" ||
		s.Labels["exported_agent"] != "agt-7" {
		t.Errorf("unexpected sample: %+v", s)
	}
	if _, ok := store.find("host_memory_total_bytes", "web-01"); !ok {
		t.Error("host metrics missing")
	}
	store.mu.Lock()
	ups := map[string]float64{}
	for _, s := range store.samples {
		if s.Name == "up" {
			ups[s.Labels["job"]] = s.Value
		}
	}
	first := store.samples[0].TS
	store.mu.Unlock()
	if ups["demo"] != 1 || ups["dead"] != 0 {
		t.Errorf("up values: %v", ups)
	}
	if age := time.Since(time.UnixMilli(first)); age < 300*time.Millisecond {
		t.Errorf("oldest stored sample is only %v old; spooled batches were not replayed", age)
	}

	waitFor(t, "log line", func() bool { store.mu.Lock(); defer store.mu.Unlock(); return len(store.logs) > 0 })
	store.mu.Lock()
	row, sf := store.logs[0], store.streamFields
	nlogs := len(store.logs)
	store.mu.Unlock()
	if row["_msg"] != "payment failed" || row["level"] != "error" || row["order"] != "42" || row["app"] != "demo" ||
		row["env"] != "test" || row["agent"] != "web-01" || row["exported_agent"] != "someone-else" || row["path"] != logFile || strings.HasPrefix(row["_time"], "1999") {
		t.Errorf("log row: %v", row)
	}
	if sf != "agent,app,env,level,path,source" {
		t.Errorf("stream fields: %q", sf)
	}
	if nlogs != 1 {
		t.Errorf("%d log rows stored, want 1", nlogs)
	}

	get := func(path, token string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, central.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := get("/api/v1/agents", "admin")
	var agents []server.AgentInfo
	if json.Unmarshal([]byte(body), &agents); code != 200 || len(agents) != 1 || agents[0].ID != "web-01" || !agents[0].Up || agents[0].Version != "test" {
		t.Errorf("agents: %d %s", code, body)
	}
	if code, _ := get("/api/v1/agents", "ingest"); code != http.StatusUnauthorized {
		t.Errorf("ingest token on admin API: %d", code)
	}
	// Query proxy reaches the store and does not leak the admin token to it.
	if code, body := get("/api/v1/query?query=up", "admin"); code != 200 || !strings.Contains(body, `"auth":""`) {
		t.Errorf("query proxy: %d %s", code, body)
	}
	if code, body := get("/api/v1/logs/query?query=error", "admin"); code != 200 || body != "logsql:error:" {
		t.Errorf("log query proxy: %d %q", code, body)
	}
	if code, _ := get("/api/v1/logs/delete", "admin"); code != http.StatusNotFound {
		t.Errorf("non-allowlisted log op proxied: %d", code)
	}
	if code, _ := get("/api/v1/logs/query?query=error", "ingest"); code != http.StatusUnauthorized {
		t.Errorf("ingest token on log query: %d", code)
	}
	if code, _ := get("/api/v1/admin/tsdb/delete_series", "admin"); code != http.StatusNotFound {
		t.Errorf("non-allowlisted path proxied: %d", code)
	}

	// Writes: wrong token, missing agent id, reserved agent id.
	post := func(token, agentID string) int {
		req, _ := http.NewRequest(http.MethodPost, central.URL+"/api/v1/write", strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+token)
		if agentID != "" {
			req.Header.Set("X-Vigil-Agent", agentID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("wrong", "x"); c != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", c)
	}
	if c := post("ingest", ""); c != http.StatusBadRequest {
		t.Errorf("no agent id: %d", c)
	}
	if c := post("ingest", "vigil-server"); c != http.StatusBadRequest {
		t.Errorf("reserved agent id: %d", c)
	}
}
