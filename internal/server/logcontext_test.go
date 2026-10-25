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
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// A line from dockerd, written in the same second as the agent restarted:
// the context says so without any AI, an explanation is made once and then
// given again from the cache, and nothing is sent when log excerpts are
// not allowed.
func TestLogLineContextAndExplain(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second).Add(65 * time.Millisecond)
	ts := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339Nano) }
	msg := `time="x" level=info msg="request cancelled by client" error="write unix /run/docker.sock->@: write: broken pipe" status=499`
	line := map[string]string{"_time": ts(0), "_msg": msg, "_stream_id": "00ab", "agent": "web-1", "unit": "docker.service", "level": "info", "source": "journald"}

	var logQueries []string
	logs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		q := r.Form.Get("query")
		logQueries = append(logQueries, q)
		enc := json.NewEncoder(w)
		switch {
		case strings.Contains(q, "| stats by (agent)"):
			enc.Encode(map[string]string{"agent": "web-1", "week": "1", "day": "1", "hour": "0", "first": ts(0), "last": ts(0)})
		case strings.Contains(q, "sort by (_time desc)"): // newest first, the line itself included
			enc.Encode(line)
			enc.Encode(map[string]string{"_time": ts(-2 * time.Second), "_msg": "container list served"})
		case strings.Contains(q, "sort by (_time)"):
			enc.Encode(line)
			enc.Encode(map[string]string{"_time": ts(3 * time.Second), "_msg": "container list served again"})
		}
	}))
	defer logs.Close()
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := []any{}
		if strings.Contains(r.URL.Query().Get("query"), "vigil_agent_start_time_seconds") {
			result = append(result, map[string]any{"metric": map[string]string{"agent": "web-1"},
				"values": [][2]any{{float64(at.Unix() + 60), fmt.Sprint(at.Unix())}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	defer metrics.Close()

	calls, sent := 0, ""
	answer := `{"assessment":"harmless","summary":"dockerd saw a client hang up","hypothesis":"the vigil agent restarted mid-request","confidence":"high",
		"claims":[{"text":"the agent started in that second","evidence":["E4"]}],"unknowns":[],"next_checks":["nothing"]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": answer}},
			"usage": map[string]any{"input_tokens": 1000, "output_tokens": 100}})
	}))
	defer api.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key-not-real"), 0o600)

	start := func(data []string) (*client, func()) {
		cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
		cfg.Storage.MetricsURL, cfg.Storage.LogsURL = metrics.URL, logs.URL
		cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-opus-5-5", Input: 4, Output: 20}}
		cfg.AI = config.AI{Provider: "anthropic", Model: "claude-opus-5-5", Effort: "low", APIKeyFile: key, BaseURL: api.URL, MonthlyBudget: 5, Data: data}
		srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		central := httptest.NewServer(srv.Handler())
		c := newClient(t, central.URL)
		c.login("admin", "admin-token-secret")
		return c, func() { central.Close(); srv.Close() }
	}
	body, _ := json.Marshal(map[string]any{"line": line})

	admin, stop := start([]string{"logs"})
	defer stop()
	code, out, _ := admin.do("POST", "/api/v1/loglines/context", string(body))
	var c server.LogContext
	if json.Unmarshal([]byte(out), &c); code != 200 || c.Source != "unit docker.service" || c.Like.Week != 1 {
		t.Fatalf("context: %d %s", code, out)
	}
	if len(c.Before) != 1 || len(c.After) != 1 || c.Before[0].Msg != "container list served" {
		t.Errorf("lines around (the line itself must not be among them): %+v %+v", c.Before, c.After)
	}
	notes := strings.Join(c.Notes, " | ")
	for _, want := range []string{"level is info", "only line like it", "started in the same second as this line"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q: %s", want, notes)
		}
	}
	if len(c.Host) != 1 || !strings.Contains(c.Host[0].What, "vigil agent") {
		t.Errorf("host events: %+v", c.Host)
	}
	if q := strings.Join(logQueries, "\n"); !strings.Contains(q, `unit:="docker.service" _msg:~`) || !strings.Contains(q, `_stream_id:00ab`) {
		t.Errorf("queries: %s", q)
	}
	if code, _, _ := admin.do("POST", "/api/v1/loglines/context", `{"line":{"_time":"yesterday","agent":"web-1","_stream_id":"00ab"}}`); code != 400 {
		t.Errorf("a line without a readable time: %d", code)
	}

	code, out, _ = admin.do("POST", "/api/v1/loglines/explain/preview", string(body))
	if code != 200 || !strings.Contains(out, `"title":"The log line"`) || !strings.Contains(out, `"cached":null`) {
		t.Fatalf("preview: %d %.600s", code, out)
	}
	var ex struct {
		ID     int64
		Cost   float64
		Cached bool
		Answer struct{ Assessment string }
	}
	code, out, _ = admin.do("POST", "/api/v1/loglines/explain", string(body))
	if json.Unmarshal([]byte(out), &ex); code != 200 || ex.Answer.Assessment != "harmless" || ex.Cached || ex.Cost <= 0 || calls != 1 {
		t.Fatalf("explain: %d calls=%d %s", code, calls, out)
	}
	if !strings.Contains(sent, "request cancelled by client") || !strings.Contains(sent, "the vigil agent on this host started") {
		t.Errorf("what was sent: %.800s", sent)
	}
	// The same line again, or one like it: from the cache, no call.
	code, out, _ = admin.do("POST", "/api/v1/loglines/explain", string(body))
	if json.Unmarshal([]byte(out), &ex); code != 200 || !ex.Cached || calls != 1 {
		t.Errorf("second explain should be cached: %d calls=%d %s", code, calls, out)
	}
	if _, out, _ = admin.do("POST", "/api/v1/loglines/explain/preview", string(body)); !strings.Contains(out, `"cached":{`) {
		t.Errorf("preview shows no earlier explanation: %.300s", out)
	}
	again, _ := json.Marshal(map[string]any{"line": line, "again": true})
	if code, _, _ = admin.do("POST", "/api/v1/loglines/explain", string(again)); code != 200 || calls != 2 {
		t.Errorf("explain again: %d calls=%d", code, calls)
	}

	// Without the logs data class nothing is sent.
	noLogs, stop2 := start(nil)
	defer stop2()
	if code, out, _ = noLogs.do("POST", "/api/v1/loglines/explain", string(body)); code != 409 || !strings.Contains(out, "ai.data") || calls != 2 {
		t.Errorf("without logs allowed: %d calls=%d %s", code, calls, out)
	}
	if _, out, _ = noLogs.do("POST", "/api/v1/loglines/explain/preview", string(body)); !strings.Contains(out, "not allowed") || strings.Contains(out, "broken pipe") {
		t.Errorf("preview without logs allowed: %.500s", out)
	}
}
