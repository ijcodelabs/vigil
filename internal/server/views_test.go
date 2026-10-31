package server_test

import (
	"encoding/json"
	"fmt"
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

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// An admin asks for a view naming a host; the model answers with a
// reference, vigil writes the host in and runs the queries; a refused
// answer leaves the draft as it was and is told to the model next time; a
// second turn changes the draft; Save makes a dashboard. The model never
// sees data, and the catalog it gets holds no host names.
func TestViews(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	record := func(q string) { mu.Lock(); queries = append(queries, q); mu.Unlock() }
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		record(q)
		if strings.Contains(q, "nosuch(") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "unknown function nosuch"})
			return
		}
		result := []any{}
		if strings.Contains(q, "node_load1") {
			result = append(result, map[string]any{"metric": map[string]string{"agent": "web-1"}, "value": [2]any{float64(time.Now().Unix()), "0.5"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	defer metrics.Close()
	logs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		record(r.Form.Get("query"))
		json.NewEncoder(w).Encode(map[string]string{"_time": time.Now().UTC().Format(time.RFC3339), "_msg": "GET / 500"})
	}))
	defer logs.Close()

	panel := func(title, typ, source, expr string) map[string]any {
		return map[string]any{"title": title, "type": typ, "source": source, "w": 6, "h": 3, "unit": "", "limit": 0,
			"queries": []any{map[string]any{"expr": expr, "legend": ""}}}
	}
	answers := []map[string]any{
		{"title": "web-1", "note": "load and errors", "question": "", "panels": []any{
			panel("Load", "timeseries", "metrics", `node_load1{agent="@h1"}`),
			panel("Errors", "logs", "logs", `agent:="@h1" "500"`)}},
		// Widens the reference: refused.
		{"title": "web-1", "note": "", "question": "", "panels": []any{panel("Load", "timeseries", "metrics", `node_load1{agent=~"@h1|.*"}`)}},
		{"title": "web-1 load", "note": "only load", "question": "", "panels": []any{
			panel("Load", "stat", "metrics", `avg(node_load1{agent="@h1"})`),
			panel("Broken", "stat", "metrics", `nosuch(up)`)}},
		{"title": "web-1 load", "note": "", "question": "", "panels": []any{panel("Load", "stat", "metrics", `avg(node_load1{agent="@h1"})`)}},
	}
	var sent []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(sent)
		sent = append(sent, string(b))
		mu.Unlock()
		text, _ := json.Marshal(answers[n])
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": string(text)}},
			"usage": map[string]any{"input_tokens": 10000, "output_tokens": 1000}})
	}))
	defer api.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key-not-real"), 0o600)

	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = metrics.URL, logs.URL
	cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-opus-5-5", Input: 4, Output: 20}}
	cfg.AI = config.AI{Provider: "anthropic", Model: "claude-opus-5-5", Effort: "low", APIKeyFile: key, BaseURL: api.URL, MonthlyBudget: 5}
	st, err := server.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Touch("web-1", "test", "192.0.2.10", "", time.Now())
	st.Close()
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	central := httptest.NewServer(srv.Handler())
	defer func() { central.Close(); srv.Close() }()
	admin := newClient(t, central.URL)
	admin.login("admin", "admin-token-secret")

	type draft struct {
		ID        int64
		Revision  int
		Dashboard server.Dashboard
		Note      string
		Checks    []struct {
			Panel, Query   int
			Status, Detail string
		}
		Changes []string
		Hosts   map[string]string
		Turns   []struct {
			Ask, Error string
			Cost       float64
		}
	}
	turn := func(path, ask string, rev int) draft {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ask": ask, "revision": rev})
		code, out, _ := admin.do("POST", path, string(body))
		var d draft
		if json.Unmarshal([]byte(out), &d); code != 200 {
			t.Fatalf("%s: %d %s", path, code, out)
		}
		return d
	}

	d := turn("/api/v1/views", "show me load and 500s on WEB-1", 0)
	if d.Revision != 1 || d.Hosts["h1"] != "web-1" || len(d.Dashboard.Panels) != 2 {
		t.Fatalf("first turn: %+v", d)
	}
	if q := d.Dashboard.Panels[0].Queries[0].Expr; q != `node_load1{agent="web-1"}` {
		t.Errorf("host not written in: %s", q)
	}
	if d.Dashboard.Panels[1].Limit != 50 || len(d.Checks) != 2 || d.Checks[0].Status != "ok" || d.Checks[1].Status != "ok" {
		t.Errorf("checks: %+v, logs limit %d", d.Checks, d.Dashboard.Panels[1].Limit)
	}
	mu.Lock()
	first := sent[0]
	mu.Unlock()
	var req struct {
		System   any
		Messages []struct{ Content []struct{ Text string } }
	}
	json.Unmarshal([]byte(first), &req)
	system, _ := json.Marshal(req.System)
	if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content[0].Text, `<host ref="h1">web-1</host>`) {
		t.Errorf("the reference was not sent: %.300s", first)
	}
	if !strings.Contains(string(system), "host_cpu_seconds_total") || strings.Contains(first, "192.0.2.10") {
		t.Errorf("the catalog is missing, or the address was sent: %d bytes", len(first))
	}

	// The widened reference is refused; the draft is as it was.
	d = turn(fmt.Sprintf("/api/v1/views/%d/turns", d.ID), "all hosts", 1)
	if d.Revision != 1 || len(d.Turns) != 2 || !strings.Contains(d.Turns[1].Error, "host reference") || len(d.Dashboard.Panels) != 2 {
		t.Fatalf("refused turn: %+v", d)
	}
	// The next turn tells the model why, and gets a broken query back.
	d = turn(fmt.Sprintf("/api/v1/views/%d/turns", d.ID), "only load, as a number", 1)
	mu.Lock()
	third := sent[2]
	mu.Unlock()
	if !strings.Contains(third, "earlier_answer_refused") || !strings.Contains(third, "current_dashboard") {
		t.Errorf("third request: %.400s", third)
	}
	if d.Revision != 2 || d.Checks[1].Status != "invalid" || len(d.Changes) == 0 {
		t.Errorf("third turn: %+v", d)
	}
	// Two tabs: an old revision is refused.
	body, _ := json.Marshal(map[string]any{"ask": "x", "revision": 1})
	if code, _, _ := admin.do("POST", fmt.Sprintf("/api/v1/views/%d/turns", d.ID), string(body)); code != 409 {
		t.Errorf("stale revision: %d", code)
	}
	// Save refuses while a query fails.
	if code, out, _ := admin.do("POST", fmt.Sprintf("/api/v1/views/%d/save", d.ID), `{}`); code != 409 || !strings.Contains(out, "invalid") {
		t.Errorf("save with a broken query: %d %s", code, out)
	}
	d = turn(fmt.Sprintf("/api/v1/views/%d/turns", d.ID), "drop the broken one", 2)
	code, out, _ := admin.do("POST", fmt.Sprintf("/api/v1/views/%d/save", d.ID), `{"title":"Web load"}`)
	if code != 200 || !strings.Contains(out, `"web-load"`) {
		t.Fatalf("save: %d %s", code, out)
	}
	if _, out, _ = admin.do("GET", "/api/v1/dashboards/web-load", ""); !strings.Contains(out, `avg(node_load1{agent=\"web-1\"})`) {
		t.Errorf("saved dashboard: %s", out)
	}
	if code, _, _ = admin.do("GET", fmt.Sprintf("/api/v1/views/%d", d.ID), ""); code != 404 {
		t.Errorf("the draft should be gone after Save: %d", code)
	}
	// Every turn is in the ledger: 4 calls at 0.04 + 0.02.
	if _, out, _ = admin.do("GET", "/api/v1/ai", ""); !strings.Contains(out, `"spent_this_month":0.24`) {
		t.Errorf("spend: %s", out)
	}
	if code, out, _ = admin.do("GET", "/api/v1/views/catalog", ""); code != 200 || strings.Contains(out, "web-1") {
		t.Errorf("catalog: %d, holds a host name: %v", code, strings.Contains(out, "web-1"))
	}
}
