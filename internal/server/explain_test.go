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
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// An admin previews what would be sent, sends it, and gets a checked
// answer; the budget and the kill switch stop further calls; anyone can
// give a verdict.
func TestExplain(t *testing.T) {
	var sent string
	answer := `{"summary":"shop-api is down","hypothesis":"killed for memory","confidence":"medium",
		"claims":[{"text":"exit code 137","evidence":["E1"]},{"text":"memory grew","evidence":["E99"]},{"text":"probably a leak","evidence":[]}],
		"unknowns":["why memory grew"],"next_checks":["look at the memory chart"]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": answer}},
			"usage": map[string]any{"input_tokens": 100000, "output_tokens": 10000}})
	}))
	defer api.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("test-key-not-real"), 0o600)

	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret",
		HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = "http://127.0.0.1:1", "http://127.0.0.1:1"
	cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-opus-5-5", Input: 4, Output: 20}}
	// One call costs 0.4 + 0.2 = 0.6; the budget allows one.
	cfg.AI = config.AI{Provider: "anthropic", Model: "claude-opus-5-5", Effort: "low", APIKeyFile: key, BaseURL: api.URL, MonthlyBudget: 0.5}
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	central := httptest.NewServer(srv.Handler())
	defer func() { central.Close(); srv.Close() }()
	admin := newClient(t, central.URL)
	admin.login("admin", "admin-token-secret")

	ctx, now := context.Background(), time.Now()
	fake := strings.Repeat("q7", 8) // built here so that no secret-looking literal sits in the source
	srv.SendAlerts(ctx, []alerting.Alert{{Labels: map[string]string{"alertname": "ContainerDown", "agent": "web-1", "container": "shop-api",
		"severity": "critical", "owner": "ada@example.com"}, Annotations: map[string]string{"summary": "token=" + fake + " shop-api stopped"},
		StartsAt: now, EndsAt: now.Add(time.Hour)}})
	srv.SyncIncidents(ctx, now)

	code, body, _ := admin.do("GET", "/api/v1/incidents/1/explain", "")
	var preview struct {
		AI     struct{ Enabled bool }
		Bundle struct {
			Items    []struct{ ID, Class, Title, Text string }
			Withheld []string
		}
	}
	if json.Unmarshal([]byte(body), &preview); code != 200 || !preview.AI.Enabled || len(preview.Bundle.Items) < 2 {
		t.Fatalf("preview: %d %s", code, body)
	}
	all, _ := json.Marshal(preview.Bundle)
	if strings.Contains(string(all), fake) || strings.Contains(string(all), "ada@example.com") {
		t.Errorf("a secret or an address is in the bundle: %s", all)
	}
	if len(preview.Bundle.Withheld) == 0 || !strings.Contains(strings.Join(preview.Bundle.Withheld, " "), "not allowed") {
		t.Errorf("logs were not withheld: %v", preview.Bundle.Withheld)
	}

	code, body, _ = admin.do("POST", "/api/v1/incidents/1/explain", "")
	var ex struct {
		ID     int64
		Cost   float64
		Answer struct {
			Confidence string
			Claims     []struct {
				Text           string
				Interpretation bool
				Unsupported    []string
			}
		}
	}
	if json.Unmarshal([]byte(body), &ex); code != 200 || ex.Answer.Confidence != "medium" || len(ex.Answer.Claims) != 3 || ex.Cost < 0.59 || ex.Cost > 0.61 {
		t.Fatalf("explain: %d %s", code, body)
	}
	if c := ex.Answer.Claims; c[0].Interpretation || len(c[0].Unsupported) != 0 || len(c[1].Unsupported) != 1 || !c[2].Interpretation {
		t.Errorf("claims not checked: %+v", c)
	}
	if !strings.Contains(sent, `evidence id=\"E1\"`) || strings.Contains(sent, fake) || !strings.Contains(sent, "json_schema") {
		t.Errorf("what was sent: %.500s", sent)
	}

	// The budget is spent: no second call.
	if code, body, _ = admin.do("POST", "/api/v1/incidents/1/explain", ""); code != 409 || !strings.Contains(body, "budget") {
		t.Errorf("over budget: %d %s", code, body)
	}
	// The kill switch.
	if code, _, _ = admin.do("PUT", "/api/v1/ai/paused", `{"paused":true}`); code != 204 {
		t.Fatalf("pause: %d", code)
	}
	if _, body, _ = admin.do("GET", "/api/v1/ai", ""); !strings.Contains(body, `"paused":true`) || !strings.Contains(body, `"spent_this_month":0.6`) {
		t.Errorf("status: %s", body)
	}
	if code, body, _ = admin.do("POST", "/api/v1/incidents/1/explain", ""); code != 409 || !strings.Contains(body, "paused") {
		t.Errorf("while paused: %d %s", code, body)
	}

	if code, body, _ = admin.do("POST", "/api/v1/explanations/1/verdict", `{"verdict":"partly","note":"it was the cron job"}`); code != 204 {
		t.Fatalf("verdict: %d %s", code, body)
	}
	if code, _, _ = admin.do("POST", "/api/v1/explanations/1/verdict", `{"verdict":"great"}`); code != 400 {
		t.Errorf("a made-up verdict: %d", code)
	}
	if _, body, _ = admin.do("GET", "/api/v1/incidents/1/explanations", ""); !strings.Contains(body, `"verdict":"partly"`) || !strings.Contains(body, `"evidence":[{`) {
		t.Errorf("stored: %.400s", body)
	}
}
