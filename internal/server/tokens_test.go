package server_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A read-only token asks what a viewer may see and changes nothing.
func TestReadOnlyTokens(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	_, central := startServer(t, stores.URL)
	c := newClient(t, central.URL)
	c.csrf = false // a tool with a token sends no such header
	with := func(token, method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer "+token)
		return code, out
	}
	admin := func(method, path, body string) (int, string) { return with("admin-token-secret", method, path, body) }

	code, body := admin("POST", "/api/v1/tokens", `{"name":" grafana ","valid_days":30}`)
	var made struct{ ID, Name, Token, Expires string }
	if err := json.Unmarshal([]byte(body), &made); code != 200 || err != nil || made.Name != "grafana" || !strings.HasPrefix(made.Token, "vgr_") || made.Expires == "" {
		t.Fatalf("make a token: %d %s", code, body)
	}
	if code, body := admin("GET", "/api/v1/tokens", ""); code != 200 || strings.Contains(body, made.Token) || !strings.Contains(body, `"name":"grafana"`) || strings.Contains(body, "last_used") {
		t.Errorf("list: %d %s", code, body)
	}
	for body, want := range map[string]int{`{"name":""}`: 400, `{"name":"x","valid_days":-1}`: 400, `{}`: 400} {
		if code, _ := admin("POST", "/api/v1/tokens", body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}

	// It reads: the query APIs by GET and by POST, the log store's own
	// paths, and what any viewer can look at.
	for _, call := range [][2]string{
		{"GET", "/api/v1/query?query=cpu_high"}, {"POST", "/api/v1/query?query=cpu_high"},
		{"POST", "/api/v1/logs/stats_query?query=error"}, {"GET", "/select/logsql/stats_query?query=error"}, {"POST", "/select/logsql/stats_query?query=error"},
		{"GET", "/api/v1/rules"}, {"GET", "/api/v1/dashboards"}, {"GET", "/api/v1/queries"}, {"GET", "/alertmanager/api/v2/alerts"},
	} {
		if code, body := with(made.Token, call[0], call[1], ""); code != 200 {
			t.Errorf("%s %s with the token: %d %s", call[0], call[1], code, body)
		}
	}
	if _, body := with(made.Token, "GET", "/select/logsql/stats_query?query=error", ""); !strings.Contains(body, `"app":"shop"`) {
		t.Errorf("the log store's path did not reach the log store: %s", body)
	}
	if code, body := with(made.Token, "GET", "/api/v1/me", ""); code != 200 || !strings.Contains(body, `"user":"token grafana"`) || !strings.Contains(body, `"role":"viewer"`) || !strings.Contains(body, `"read_only":true`) {
		t.Errorf("who the token is: %d %s", code, body)
	}
	// Who is told about alerts is for admins; the status says so to nobody else.
	if code, body := with(made.Token, "GET", "/alertmanager/api/v2/status", ""); code != 200 || strings.Contains(body, "receivers") || !strings.Contains(body, "versionInfo") {
		t.Errorf("alerting status for a viewer: %d %s", code, body)
	}
	if code, body := admin("GET", "/alertmanager/api/v2/status", ""); code != 200 || !strings.Contains(body, "receivers") {
		t.Errorf("alerting status for an admin: %d %.200s", code, body)
	}
	if code, _ := with(made.Token, "GET", "/select/logsql/delete", ""); code != 404 {
		t.Errorf("an operation that is not a query: %d", code)
	}

	// It changes nothing, not even what a viewer may change, and sees
	// nothing that is for admins.
	for _, call := range [][3]string{
		{"POST", "/api/v1/queries", `{"name":"x","query":"up"}`}, {"POST", "/api/v1/me/password", `{}`},
		{"PUT", "/api/v1/rules/config", "groups: []"}, {"POST", "/api/v1/tokens", `{"name":"more"}`},
		{"DELETE", "/api/v1/tokens/" + made.ID, ""}, {"GET", "/api/v1/tokens", ""}, {"GET", "/api/v1/users", ""},
		{"POST", "/alertmanager/api/v2/silences", `{}`}, {"POST", "/api/v1/write", ""},
	} {
		if code, body := with(made.Token, call[0], call[1], call[2]); code != 403 && code != 401 {
			t.Errorf("%s %s with the token: %d %s", call[0], call[1], code, body)
		}
	}
	if code, body := with(made.Token, "POST", "/api/v1/queries", `{"name":"x","query":"up"}`); code != 403 || !strings.Contains(body, "only reads") {
		t.Errorf("a write says why it was refused: %d %s", code, body)
	}

	// The list shows it was used; a wrong or revoked token is nobody.
	if _, body := admin("GET", "/api/v1/tokens", ""); !strings.Contains(body, "last_used") {
		t.Errorf("use was not noted: %s", body)
	}
	if code, _ := with("vgr_0000000000000000", "GET", "/api/v1/rules", ""); code != 401 {
		t.Errorf("a made-up token: %d", code)
	}
	if code, _ := admin("DELETE", "/api/v1/tokens/"+made.ID, ""); code != 204 {
		t.Errorf("revoke: %d", code)
	}
	if code, _ := with(made.Token, "GET", "/api/v1/rules", ""); code != 401 {
		t.Errorf("a revoked token: %d", code)
	}
}
