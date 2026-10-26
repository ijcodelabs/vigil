package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// client is a browser-like API client: it keeps cookies and can send the
// CSRF header the UI always sends.
type client struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf bool
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, hc: &http.Client{Jar: jar}, csrf: true}
}

func (c *client) do(method, path, body string, headers ...string) (int, string, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if c.csrf {
		req.Header.Set("X-Vigil-CSRF", "1")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b)), resp.Header
}

func (c *client) login(user, pass string) int {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	code, _, _ := c.do("POST", "/api/v1/login", string(body))
	return code
}

func startServer(t *testing.T, storeURL string) (*server.Server, *httptest.Server) {
	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret",
		HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = storeURL, storeURL
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	central := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { central.Close(); srv.Close() })
	return srv, central
}

func TestUsersAndSessions(t *testing.T) {
	_, central := startServer(t, "http://127.0.0.1:1")
	c := newClient(t, central.URL)

	// The UI itself is public, the API is not.
	if code, body, h := c.do("GET", "/", ""); code != 200 || !strings.Contains(body, "<title>") || h.Get("Content-Security-Policy") == "" {
		t.Errorf("index: %d %q", code, body)
	}
	if code, _, _ := c.do("GET", "/api/v1/me", ""); code != 401 {
		t.Errorf("anonymous /me: %d", code)
	}
	if code, _, _ := c.do("GET", "/api/v1/rules", ""); code != 401 {
		t.Errorf("anonymous rules: %d", code)
	}

	// Fresh server: admin + the admin token signs in.
	if code := c.login("admin", "nope"); code != 401 {
		t.Errorf("wrong bootstrap password: %d", code)
	}
	if code := c.login("admin", "admin-token-secret"); code != 204 {
		t.Fatalf("bootstrap login: %d", code)
	}
	if code, body, _ := c.do("GET", "/api/v1/me", ""); code != 200 || !strings.Contains(body, `"bootstrap":true`) || !strings.Contains(body, `"version":`) {
		t.Errorf("/me: %d %s", code, body)
	}

	// A cookie alone must not be enough to change things.
	c.csrf = false
	if code, body, _ := c.do("PUT", "/api/v1/users/eve", `{"password":"longenough-pw","role":"admin"}`); code != 403 || !strings.Contains(body, "CSRF") {
		t.Errorf("no CSRF header: %d %s", code, body)
	}
	c.csrf = true

	for body, want := range map[string]string{
		`{"password":"longenough-pw","role":"viewer"}`: "first account must be an admin",
		`{"password":"short","role":"admin"}`:          "at least 10",
		`{"password":"longenough-pw","role":"root"}`:   "admin or viewer",
	} {
		if code, msg, _ := c.do("PUT", "/api/v1/users/nora", body); code != 400 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s", body, code, msg)
		}
	}
	if code, msg, _ := c.do("PUT", "/api/v1/users/nora", `{"password":"longenough-pw","role":"admin"}`); code != 204 {
		t.Fatalf("create admin: %d %s", code, msg)
	}
	// With a real account present, the bootstrap session and login stop working.
	if code, _, _ := c.do("GET", "/api/v1/me", ""); code != 401 {
		t.Errorf("bootstrap session after first user: %d", code)
	}
	if code := c.login("admin", "admin-token-secret"); code != 401 {
		t.Errorf("bootstrap login after first user: %d", code)
	}

	if code := c.login("nora", "longenough-pw"); code != 204 {
		t.Fatalf("user login: %d", code)
	}
	if code, msg, _ := c.do("PUT", "/api/v1/users/vera", `{"password":"another-long-pw","role":"viewer"}`); code != 204 {
		t.Fatalf("create viewer: %d %s", code, msg)
	}
	if code, msg, _ := c.do("PUT", "/api/v1/users/nora", `{"role":"viewer"}`); code != 409 {
		t.Errorf("demoting the only admin: %d %s", code, msg)
	}
	if code, _, _ := c.do("DELETE", "/api/v1/users/nora", ""); code != 409 {
		t.Errorf("deleting the only admin: %d", code)
	}

	v := newClient(t, central.URL)
	if code := v.login("vera", "another-long-pw"); code != 204 {
		t.Fatalf("viewer login: %d", code)
	}
	if code, _, _ := v.do("GET", "/api/v1/rules", ""); code != 200 {
		t.Errorf("viewer reads rules: %d", code)
	}
	for _, req := range [][2]string{{"PUT", "/api/v1/rules/config"}, {"GET", "/api/v1/users"}, {"GET", "/api/v1/alerting/config"},
		{"POST", "/alertmanager/api/v2/silences"}, {"PUT", "/api/v1/fleet/profiles/x"}, {"POST", "/api/v1/agents/x/token"}} {
		if code, _, _ := v.do(req[0], req[1], "{}"); code != 403 {
			t.Errorf("viewer %s %s: %d, want 403", req[0], req[1], code)
		}
	}
	if code, _, _ := v.do("GET", "/alertmanager/api/v2/silences", ""); code != 200 {
		t.Errorf("viewer lists silences: %d", code)
	}
	if code, msg, _ := v.do("POST", "/api/v1/me/password", `{"old":"another-long-pw","new":"a-brand-new-password"}`); code != 204 {
		t.Errorf("own password: %d %s", code, msg)
	}
	if code := newClient(t, central.URL).login("vera", "a-brand-new-password"); code != 204 {
		t.Errorf("login with new password: %d", code)
	}
	if code, _, _ := v.do("POST", "/api/v1/logout", ""); code != 204 {
		t.Errorf("logout: %d", code)
	}
	if code, _, _ := v.do("GET", "/api/v1/me", ""); code != 401 {
		t.Errorf("after logout: %d", code)
	}
	// Deleting a user ends their sessions.
	v.login("vera", "a-brand-new-password")
	c.do("DELETE", "/api/v1/users/vera", "")
	if code, _, _ := v.do("GET", "/api/v1/me", ""); code != 401 {
		t.Errorf("deleted user still signed in: %d", code)
	}

	// Guessing is cut off.
	g := newClient(t, central.URL)
	last := 0
	for range 12 {
		last = g.login("nora", "wrong-password")
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after 12 bad logins: %d", last)
	}
}

func TestAgentTokensAndProfiles(t *testing.T) {
	store := &fakeStore{}
	backend := httptest.NewServer(store)
	defer backend.Close()
	_, central := startServer(t, backend.URL)
	admin := func(method, path, body string, h ...string) (int, string, http.Header) {
		c := &client{t: t, base: central.URL, hc: http.DefaultClient}
		return c.do(method, path, body, append(h, "Authorization", "Bearer admin-token-secret")...)
	}
	// poll is what an agent sends: who it is and what it runs.
	poll := func(token, id, report string, h ...string) (int, string, http.Header) {
		c := &client{t: t, base: central.URL, hc: http.DefaultClient}
		return c.do("POST", "/api/v1/agent/config", report, append(h, "Authorization", "Bearer "+token, "X-Vigil-Agent", id)...)
	}

	// A per-agent token works for its agent only.
	code, body, _ := admin("POST", "/api/v1/agents/db-01/token", "")
	var issued struct{ Token string }
	json.Unmarshal([]byte(body), &issued)
	if code != 200 || !strings.HasPrefix(issued.Token, "vgl_") {
		t.Fatalf("issue token: %d %s", code, body)
	}
	if code, _, _ := poll(issued.Token, "db-01", "{}"); code != 204 {
		t.Errorf("own token: %d", code)
	}
	if code, msg, _ := poll(issued.Token, "db-02", "{}"); code != 403 || !strings.Contains(msg, "different agent") {
		t.Errorf("token used for another agent: %d %s", code, msg)
	}
	if code, _, _ := poll("vgl_made-up", "db-01", "{}"); code != 401 {
		t.Errorf("unknown token: %d", code)
	}
	if code, _, _ := poll("", "db-01", "{}"); code != 401 {
		t.Errorf("no token: %d", code)
	}
	admin("DELETE", "/api/v1/agents/db-01/token", "")
	if code, _, _ := poll(issued.Token, "db-01", "{}"); code != 401 {
		t.Errorf("revoked token: %d", code)
	}

	// Three agents report in; none has a profile yet.
	for id, report := range map[string]string{
		"db-01":  `{"labels":{"env":"prod","role":"db","os":"plan9"},"facts":{"os":"linux","kernel":"6.8.0"}}`,
		"web-01": `{"labels":{"env":"prod","role":"web"},"facts":{"os":"linux","kernel":"4.18.0"}}`,
		"dev-01": `{"labels":{"env":"dev","role":"web"},"facts":{"os":"linux","kernel":"6.8.0"}}`,
	} {
		if code, _, _ := poll("ingest", id, report); code != 204 {
			t.Fatalf("%s first poll: %d", id, code)
		}
	}

	// Profiles are checked as written...
	for doc, want := range map[string]string{
		"select: ['env prod']":                           "select",
		"config: {scrape: {statik: []}}":                 "statik",
		"config: {server: {url: http://evil}}":           "server",
		"config: {interval: 10ms}":                       "at least 1s",
		"config: {logs: {templates: {t: {parse: xml}}}}": "unknown parse",
	} {
		if code, msg, _ := admin("PUT", "/api/v1/fleet/profiles/bad", doc); code != 400 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s", doc, code, msg)
		}
	}
	// ...and against the agents they would reach: this one is fine as text,
	// but gives every agent a log source whose template nobody defines.
	if code, msg, _ := admin("PUT", "/api/v1/fleet/profiles/bad", "config: {logs: {files: [{name: x, paths: [/x.log], template: nope}]}}"); code != 400 ||
		!strings.Contains(msg, "agent db-01 and 2 more") || !strings.Contains(msg, `unknown log template "nope"`) {
		t.Errorf("broken for agents: %d %s", code, msg)
	}

	put := func(name, doc string) {
		t.Helper()
		if code, msg, _ := admin("PUT", "/api/v1/fleet/profiles/"+name, doc); code != 200 {
			t.Fatalf("save %s: %d %s", name, code, msg)
		}
	}
	put("base", "priority: 0\nconfig:\n  interval: 30s\n  scrape:\n    static:\n      - {job: node, targets: ['localhost:9100']}\n")
	put("prod", "select: ['env=\"prod\"']\npriority: 10\nconfig:\n  interval: 15s\n")
	put("databases", "select: ['role=\"db\"', 'os=\"linux\"']\npriority: 20\nconfig:\n  scrape:\n    static:\n      - {job: postgres, targets: ['localhost:9187']}\n      - {job: node, interval: 5s}\n")

	// The list shows how many agents each profile reaches. db-01's "os"
	// label did not override the fact, so the database profile still matches it.
	_, body, _ = admin("GET", "/api/v1/fleet/profiles", "")
	var profiles []struct {
		Name    string
		Matches int
		Version int
		Author  string
	}
	json.Unmarshal([]byte(body), &profiles)
	got := map[string]int{}
	for _, p := range profiles {
		got[p.Name] = p.Matches
	}
	if len(profiles) != 3 || profiles[0].Name != "base" || got["base"] != 3 || got["prod"] != 2 || got["databases"] != 1 || profiles[0].Author != "admin-token" {
		t.Errorf("profiles: %s", body)
	}
	if code, body, _ := admin("POST", "/api/v1/fleet/match", `{"select":["kernel=~\"6\\\\..*\""]}`); code != 200 || !strings.Contains(body, `"matched":2`) || !strings.Contains(body, `"of":3`) {
		t.Errorf("match preview: %d %s", code, body)
	}
	if code, body, _ := admin("POST", "/api/v1/fleet/match", `{"select":["role=\"none\""]}`); code != 200 || !strings.Contains(body, `"matched":0`) || !strings.Contains(body, `"agents":[]`) {
		t.Errorf("match preview of nothing: %d %s", code, body)
	}

	// Each agent gets its own merge.
	code, cfgDB, h := poll("ingest", "db-01", `{"labels":{"env":"prod","role":"db"},"facts":{"os":"linux","kernel":"6.8.0"}}`)
	if code != 200 || !strings.Contains(cfgDB, "interval: 15s") || !strings.Contains(cfgDB, "job: postgres") || !strings.Contains(cfgDB, "interval: 5s") {
		t.Errorf("db-01 config: %d\n%s", code, cfgDB)
	}
	if code, _, _ := poll("ingest", "db-01", `{"labels":{"env":"prod","role":"db"},"facts":{"os":"linux","kernel":"6.8.0"}}`, "If-None-Match", h.Get("ETag")); code != http.StatusNotModified {
		t.Errorf("unchanged: %d", code)
	}
	if _, cfgDev, _ := poll("ingest", "dev-01", `{"labels":{"env":"dev","role":"web"},"facts":{"os":"linux"}}`); !strings.Contains(cfgDev, "interval: 30s") || strings.Contains(cfgDev, "postgres") {
		t.Errorf("dev-01 config:\n%s", cfgDev)
	}
	// The effective-config view says where each part comes from.
	_, body, _ = admin("GET", "/api/v1/agents/db-01/config", "")
	var eff struct {
		Source   string
		Profiles []string
		Origins  []struct {
			Path     string
			Profiles []string
		}
	}
	json.Unmarshal([]byte(body), &eff)
	origin := map[string]string{}
	for _, o := range eff.Origins {
		origin[o.Path] = strings.Join(o.Profiles, ">")
	}
	if eff.Source != "managed" || strings.Join(eff.Profiles, ",") != "base,prod,databases" || origin["interval"] != "base>prod" || origin["scrape.static[node]"] != "base>databases" {
		t.Errorf("effective config: %s", body)
	}

	// History: a second version, the old one readable, and a delete recorded.
	put("prod", "select: ['env=\"prod\"']\npriority: 10\nconfig:\n  interval: 20s\n")
	_, body, _ = admin("GET", "/api/v1/fleet/profiles/prod/versions", "")
	if !strings.Contains(body, `"version":2`) || !strings.Contains(body, `"version":1`) {
		t.Errorf("versions: %s", body)
	}
	if code, old, _ := admin("GET", "/api/v1/fleet/profiles/prod/versions/1", ""); code != 200 || !strings.Contains(old, "interval: 15s") {
		t.Errorf("version 1: %d %s", code, old)
	}
	// Removing the base would leave the database profile overriding a job nobody defines.
	if code, msg, _ := admin("DELETE", "/api/v1/fleet/profiles/base", ""); code != 400 || !strings.Contains(msg, "targets is empty") || !strings.Contains(msg, "db-01") {
		t.Errorf("delete that breaks an agent: %d %s", code, msg)
	}
	if code, _, _ := admin("DELETE", "/api/v1/fleet/profiles/prod", ""); code != 204 {
		t.Errorf("delete prod: %d", code)
	}
	_, body, _ = admin("GET", "/api/v1/fleet/profiles/prod/versions", "")
	if !strings.Contains(body, `"version":3`) || !strings.Contains(body, `"deleted":true`) {
		t.Errorf("delete not in history: %s", body)
	}
	for _, name := range []string{"databases", "base"} {
		admin("DELETE", "/api/v1/fleet/profiles/"+name, "")
	}

	// A real agent: its file has a label and no log sources; a profile
	// selected by that label adds one. The agent's own labels stay on the data.
	logFile := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(logFile, nil, 0o644)
	acfg := &config.Agent{AgentID: "web-09", SpoolDir: t.TempDir(), SpoolMaxBytes: 1 << 20}
	acfg.Server.URL, acfg.Server.Token = central.URL, "ingest"
	local, _ := config.ParseCollection("host_metrics: true\nlabels: {site: zagreb}\n")
	acfg.Collection = *local
	acfg.Interval = config.Duration(200 * time.Millisecond)
	on := true
	acfg.RemoteConfig = &on
	a, err := agent.New(acfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.PollEvery = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	fleetAgent := func() map[string]any {
		_, body, _ := admin("GET", "/api/v1/agents", "")
		var list []map[string]any
		json.Unmarshal([]byte(body), &list)
		for _, ag := range list {
			if ag["id"] == "web-09" {
				return ag
			}
		}
		return map[string]any{}
	}
	waitFor(t, "agent to report with its local config", func() bool {
		ag := fleetAgent()
		labels, _ := ag["labels"].(map[string]any)
		facts, _ := ag["facts"].(map[string]any)
		return ag["config_source"] == "local" && ag["config_state"] == "in sync" && labels["site"] == "zagreb" && facts["os"] != nil && facts["version"] == "test"
	})

	put("zagreb", "select: ['site=\"zagreb\"']\nconfig:\n  interval: 1s\n  labels: {managed: \"yes\", site: overridden}\n  logs:\n    files:\n      - {name: app, paths: ["+logFile+"], labels: {app: managed}}\n")
	waitFor(t, "agent to apply the profile", func() bool {
		ag := fleetAgent()
		return ag["config_source"] == "managed" && ag["config_state"] == "in sync"
	})
	f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("hello from a managed source\n")
	f.Close()
	waitFor(t, "log line from the managed source", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, row := range store.logs {
			if row["_msg"] == "hello from a managed source" && row["app"] == "managed" && row["managed"] == "yes" && row["site"] == "zagreb" {
				return true
			}
		}
		return false
	})

	// Removing the profile sends the agent back to its own file.
	admin("DELETE", "/api/v1/fleet/profiles/zagreb", "")
	waitFor(t, "agent back on its local config", func() bool {
		ag := fleetAgent()
		return ag["config_source"] == "local" && ag["config_state"] == "in sync"
	})
	if _, err := os.Stat(filepath.Join(acfg.SpoolDir, "managed-config.yaml")); !os.IsNotExist(err) {
		t.Errorf("cached managed config not removed: %v", err)
	}
	if code, _, _ := admin("DELETE", "/api/v1/agents/web-09", ""); code != 204 {
		t.Errorf("forget agent: %d", code)
	}
}
