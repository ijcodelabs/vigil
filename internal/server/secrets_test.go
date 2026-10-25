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

// An admin sets up OpenBao in the interface and chooses the secret the AI
// key comes from; Explain then uses that key. No API ever answers with the
// value, the saved login is not reused for another address, and the
// password is asked for again.
func TestSecretStoreFeedsTheAIKey(t *testing.T) {
	key := "sk-ant-" + strings.Repeat("x", 20) + "wxyz" // built here, so nothing secret-looking sits in the source
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := strings.TrimPrefix(r.URL.Path, "/v1/"); {
		case p == "auth/approle/login" && strings.Contains(string(body), `"secret_id":"bao-secret-id-1"`):
			reply(map[string]any{"auth": map[string]any{"client_token": "s.t", "lease_duration": 3600, "renewable": true, "policies": []string{"vigil"}}})
		case p == "auth/approle/login":
			w.WriteHeader(400)
			reply(map[string]any{"errors": []string{"invalid secret id"}})
		case r.Header.Get("X-Vault-Token") != "s.t":
			w.WriteHeader(403)
		case p == "auth/token/lookup-self":
			reply(map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
		case p == "sys/internal/ui/mounts/vigil":
			reply(map[string]any{"data": map[string]any{"options": map[string]string{"version": "2"}}})
		case p == "sys/capabilities-self":
			reply(map[string]any{"vigil/metadata/": []string{"list"}, "vigil/data/anthropic": []string{"read"}})
		case p == "vigil/metadata/":
			reply(map[string]any{"data": map[string]any{"keys": []string{"anthropic"}}})
		case p == "vigil/data/anthropic":
			reply(map[string]any{"data": map[string]any{"data": map[string]any{"api_key": key}, "metadata": map[string]any{"version": 1}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer bao.Close()
	var gotKey string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "has_more": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "m", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": `{"summary":"s","hypothesis":"h","confidence":"low","claims":[],"unknowns":[],"next_checks":[]}`}},
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 10}})
	}))
	defer model.Close()

	dataDir := t.TempDir()
	cfg := &config.Server{DataDir: dataDir, IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = "http://127.0.0.1:1", "http://127.0.0.1:1"
	cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-opus-5-5", Input: 4, Output: 20}}
	cfg.AI = config.AI{Provider: "anthropic", Model: "claude-opus-5-5", Effort: "low", BaseURL: model.URL, MonthlyBudget: 5} // no api_key_file
	start := func() (*server.Server, *client, func()) {
		srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		central := httptest.NewServer(srv.Handler())
		c := newClient(t, central.URL)
		c.login("admin", "admin-token-secret")
		return srv, c, func() { central.Close(); srv.Close() }
	}
	srv, admin, stop := start()

	store := map[string]any{"kind": "openbao", "address": bao.URL, "auth": "approle", "role_id": "bao-role-id-1", "mount": "vigil"}
	body := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	code, out, _ := admin.do("POST", "/api/v1/secrets/store/test", body(map[string]any{"store": store, "credential": "bao-secret-id-1"}))
	if code != 200 || !strings.Contains(out, `"up":true`) || !strings.Contains(out, `"list":true`) {
		t.Fatalf("test: %d %s", code, out)
	}
	if code, out, _ = admin.do("POST", "/api/v1/secrets/store/test", body(map[string]any{"store": store, "credential": "wrong-secret-id"})); !strings.Contains(out, "invalid secret id") {
		t.Errorf("a wrong secret_id: %d %s", code, out)
	}
	if code, _, _ = admin.do("PUT", "/api/v1/secrets/store", body(map[string]any{"store": store, "credential": "bao-secret-id-1", "password": "guess"})); code != 403 {
		t.Errorf("saved without the password: %d", code)
	}
	if code, out, _ = admin.do("PUT", "/api/v1/secrets/store", body(map[string]any{"store": store, "credential": "bao-secret-id-1", "password": "admin-token-secret"})); code != 204 {
		t.Fatalf("save: %d %s", code, out)
	}
	if code, out, _ = admin.do("PUT", "/api/v1/secrets/uses/ai_api_key", `{"path":"anthropic","field":"api_key"}`); code != 204 {
		t.Fatalf("choose: %d %s", code, out)
	}
	_, out, _ = admin.do("GET", "/api/v1/secrets", "")
	if !strings.Contains(out, `"found":true`) || !strings.Contains(out, `"hint":"…wxyz"`) || strings.Contains(out, key) || strings.Contains(out, "bao-secret-id-1") {
		t.Errorf("secrets page data: %s", out)
	}
	if _, out, _ = admin.do("POST", "/api/v1/secrets/uses/ai_api_key/test", ""); !strings.Contains(out, `"provider":"accepted"`) || gotKey != key {
		t.Errorf("use test: %s (key sent: %v)", out, gotKey == key)
	}
	if _, out, _ = admin.do("GET", "/api/v1/secrets/store/list", ""); strings.TrimSpace(out) != `["anthropic"]` {
		t.Errorf("list: %q", out)
	}

	// The saved login is reused for the same store, never for another address.
	moved := map[string]any{"kind": "openbao", "address": "http://127.0.0.1:9", "auth": "approle", "role_id": "bao-role-id-1", "mount": "vigil"}
	if code, out, _ = admin.do("POST", "/api/v1/secrets/store/test", body(map[string]any{"store": moved})); code != 400 || !strings.Contains(out, "again") {
		t.Errorf("credential reused for another address: %d %s", code, out)
	}
	if code, out, _ = admin.do("POST", "/api/v1/secrets/store/test", body(map[string]any{"store": store})); code != 200 || !strings.Contains(out, `"up":true`) {
		t.Errorf("credential not reused for the same store: %d %s", code, out)
	}

	// Explain uses the key from the store.
	ctx, now := context.Background(), time.Now()
	srv.SendAlerts(ctx, []alerting.Alert{{Labels: map[string]string{"alertname": "X", "agent": "web-1", "severity": "warning"}, StartsAt: now, EndsAt: now.Add(time.Hour)}})
	srv.SyncIncidents(ctx, now)
	gotKey = ""
	if code, out, _ = admin.do("POST", "/api/v1/incidents/1/explain", ""); code != 200 || gotKey != key {
		t.Errorf("explain with the store's key: %d %.200s", code, out)
	}

	// The database holds the login only sealed; it opens again after a restart.
	stop()
	files, _ := filepath.Glob(filepath.Join(dataDir, "*")) // the database, its WAL, the key file
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		if strings.Contains(string(raw), "bao-secret-id-1") || strings.Contains(string(raw), key) {
			t.Errorf("the secret_id or the key is in %s in clear", filepath.Base(f))
		}
	}
	_, admin2, stop2 := start()
	defer stop2()
	if _, out, _ = admin2.do("GET", "/api/v1/secrets", ""); !strings.Contains(out, `"found":true`) {
		t.Errorf("after a restart: %s", out)
	}
	if code, _, _ = admin2.do("POST", "/api/v1/secrets/store/remove", `{"password":"admin-token-secret"}`); code != 204 {
		t.Errorf("remove: %d", code)
	}
	if _, out, _ = admin2.do("GET", "/api/v1/secrets", ""); !strings.Contains(out, `"store":null`) {
		t.Errorf("after remove: %s", out)
	}
}
