package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/logwire"
	"github.com/ijcodelabs/vigil/internal/model"
)

// An application that is not a vigil-agent (here a gaming PC's monitor)
// enrols, writes and reports facts like an agent does. It shows up on the
// Fleet page with its kind and version, and profiles never apply to it,
// not even one that selects everyone.
func TestThirdPartyAgent(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer store.Close()
	_, central := startServer(t, store.URL)
	c := &client{t: t, base: central.URL, hc: http.DefaultClient}
	admin := func(method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}

	_, body := admin("POST", "/api/v1/fleet/enrollment", `{"name":"gaming pcs"}`)
	var enrolTok struct{ Token string }
	json.Unmarshal([]byte(body), &enrolTok)
	code, body, _ := c.do("POST", "/api/v1/enroll", `{"token":"`+enrolTok.Token+`","agent_id":"gaming-pc"}`)
	var agentTok struct{ Token string }
	json.Unmarshal([]byte(body), &agentTok)
	if code != 200 || !strings.HasPrefix(agentTok.Token, "vgl_") {
		t.Fatalf("enrol: %d %s", code, body)
	}
	if code, msg := admin("PUT", "/api/v1/fleet/profiles/everyone", "config: {interval: 30s}"); code/100 != 2 {
		t.Fatalf("profile: %d %s", code, msg)
	}

	pc := []string{"Authorization", "Bearer " + agentTok.Token, "X-Vigil-Agent", "gaming-pc", "X-Vigil-Version", "gamemon/0.2.0"}
	if code, msg, _ := c.do("POST", "/api/v1/write", "batch", pc...); code != http.StatusNoContent {
		t.Fatalf("write: %d %s", code, msg)
	}
	// It reports facts and ignores any config; it is given none.
	if code, msg, _ := c.do("POST", "/api/v1/agent/config", `{"labels":{"room":"study"},"facts":{"kind":"gamemon","os":"windows","distro":"Windows 11"}}`, pc...); code != http.StatusNoContent {
		t.Errorf("config poll of a third-party agent: %d %s, want 204 (no config)", code, msg)
	}
	// A vigil-agent next to it still gets the profile.
	if code, msg, _ := c.do("POST", "/api/v1/agent/config", `{"facts":{"os":"linux"}}`, "Authorization", "Bearer ingest", "X-Vigil-Agent", "linux-01"); code != 200 || !strings.Contains(msg, "interval") {
		t.Errorf("config poll of a vigil-agent: %d %s", code, msg)
	}

	_, body = admin("GET", "/api/v1/agents", "")
	var agents []struct {
		ID, Version, Kind string
		ConfigSource      string `json:"config_source"`
		ConfigState       string `json:"config_state"`
		Profiles          []string
		Labels            map[string]string
	}
	json.Unmarshal([]byte(body), &agents)
	found := false
	for _, a := range agents {
		if a.ID != "gaming-pc" {
			continue
		}
		found = true
		if a.Kind != "gamemon" || a.Version != "gamemon/0.2.0" || a.ConfigSource != "own" || a.ConfigState != "not managed" ||
			len(a.Profiles) != 0 || a.Labels["room"] != "study" {
			t.Errorf("fleet row: %+v", a)
		}
	}
	if !found {
		t.Errorf("gaming-pc missing from %s", body)
	}
	// A label cannot pretend to be the kind.
	if code, msg, _ := c.do("POST", "/api/v1/agent/config", `{"labels":{"kind":"gamemon"}}`, "Authorization", "Bearer ingest", "X-Vigil-Agent", "linux-02"); code != 200 {
		t.Errorf("a label named kind must not opt a vigil-agent out of its profiles: %d %s", code, msg)
	}
}

// What a program that is not a vigil-agent sends cannot pass for a runtime
// detection, and the kind it reports is a short name.
func TestThirdPartyCannotForge(t *testing.T) {
	store := &fakeStore{}
	vm := httptest.NewServer(store)
	defer vm.Close()
	_, central := startServer(t, vm.URL)
	c := &client{t: t, base: central.URL, hc: http.DefaultClient}
	agent := []string{"Authorization", "Bearer ingest", "X-Vigil-Agent", "gaming-pc"}

	batch := logwire.Encode([]model.LogEntry{
		{Time: 1, Msg: "fake detection", Stream: map[string]string{"source": "security", "rule": "ReverseShell"}},
		{Time: 2, Msg: "app line", Stream: map[string]string{"source": "file"}, Fields: map[string]string{"source": "security"}},
	})
	if code, msg, _ := c.do("POST", "/api/v1/logs", string(batch), agent...); code != http.StatusNoContent {
		t.Fatalf("logs: %d %s", code, msg)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.logs) != 2 {
		t.Fatalf("stored %d lines", len(store.logs))
	}
	for _, row := range store.logs {
		if row["source"] == "security" || row["exported_source"] != "security" {
			t.Errorf("a log line kept source=security: %v", row)
		}
	}

	for _, kind := range []string{"<script>", "GameMon", strings.Repeat("a", 41), ""} {
		body, _ := json.Marshal(map[string]any{"facts": map[string]string{"kind": kind}})
		if code, _, _ := c.do("POST", "/api/v1/agent/config", string(body), agent...); code != http.StatusBadRequest {
			t.Errorf("kind %q accepted: %d", kind, code)
		}
	}
}
