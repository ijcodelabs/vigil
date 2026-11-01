package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
)

// An issuer token issues grants and reads nothing; a grant reads one
// incident's evidence, masked, a bounded number of times, and nothing else.
func TestEvidenceGrants(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	srv, central := startServer(t, stores.URL)
	c := newClient(t, central.URL)
	c.csrf = false
	with := func(token, method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer "+token)
		return code, out
	}
	admin := func(method, path, body string) (int, string) { return with("admin-token-secret", method, path, body) }

	ctx, now := context.Background(), time.Now()
	fake := strings.Repeat("q7", 8)
	labels := map[string]string{"alertname": "ContainerDown", "agent": "web-1", "container": "shop-api", "severity": "critical", "owner": "ada@example.com"}
	srv.SendAlerts(ctx, []alerting.Alert{{Labels: labels, Annotations: map[string]string{"summary": "token=" + fake + " shop-api stopped"},
		StartsAt: now, EndsAt: now.Add(time.Hour)}})
	srv.SyncIncidents(ctx, now)

	code, body := admin("POST", "/api/v1/tokens", `{"name":"boop-chat","scope":"issue"}`)
	var issuer struct{ Token, Scope string }
	if json.Unmarshal([]byte(body), &issuer); code != 200 || issuer.Scope != "issue" || !strings.HasPrefix(issuer.Token, "vgr_") {
		t.Fatalf("issuer token: %d %s", code, body)
	}
	if code, _ := admin("POST", "/api/v1/tokens", `{"name":"x","scope":"write"}`); code != 400 {
		t.Errorf("unknown scope: %d", code)
	}
	_, body = admin("POST", "/api/v1/tokens", `{"name":"grafana"}`)
	var reader struct{ Token, Scope string }
	if json.Unmarshal([]byte(body), &reader); reader.Scope != "read" {
		t.Errorf("default scope: %s", body)
	}

	// The issuer token reads nothing, not even as a viewer.
	for _, path := range []string{"/api/v1/query?query=up", "/api/v1/incidents/1", "/api/v1/tokens", "/api/v1/evidence-grants", "/api/v1/evidence"} {
		if code, body := with(issuer.Token, "GET", path, ""); code != 403 && code != 401 {
			t.Errorf("issuer token on %s: %d %s", path, code, body)
		}
	}
	// A read-only token cannot issue.
	if code, _ := with(reader.Token, "POST", "/api/v1/evidence-grants", `{"incident":1,"purpose":"x"}`); code != 403 {
		t.Errorf("read token issued: %d", code)
	}

	lb, _ := json.Marshal(labels)
	for in, want := range map[string]int{
		`{"purpose":"run 1"}`: 400,
		`{"incident":1,"labels":` + string(lb) + `,"purpose":"run 1"}`: 400,
		`{"incident":1}`: 400,
		`{"incident":1,"purpose":"run 1","ttl_minutes":61}`:  400,
		`{"incident":99,"purpose":"run 1"}`:                  404,
		`{"labels":{"alertname":"Other"},"purpose":"run 1"}`: 404,
	} {
		if code, body := with(issuer.Token, "POST", "/api/v1/evidence-grants", in); code != want {
			t.Errorf("%s: %d %s, want %d", in, code, body, want)
		}
	}
	// Found by the labels Alertmanager's webhook carries.
	code, body = with(issuer.Token, "POST", "/api/v1/evidence-grants", `{"labels":`+string(lb)+`,"purpose":"workroom run 1"}`)
	var g struct {
		ID, Token string
		Incident  int64
		Expires   time.Time
	}
	if json.Unmarshal([]byte(body), &g); code != 200 || g.Incident != 1 || !strings.HasPrefix(g.Token, "vge_") ||
		g.Expires.Sub(now) < 29*time.Minute || g.Expires.Sub(now) > 31*time.Minute {
		t.Fatalf("issue: %d %s", code, body)
	}

	code, body = with(g.Token, "GET", "/api/v1/evidence", "")
	var ev struct {
		Incident struct {
			ID    int64
			Title string
		}
		Bundle struct {
			Items []struct{ ID, Class, Text string }
		}
	}
	if json.Unmarshal([]byte(body), &ev); code != 200 || ev.Incident.ID != 1 || len(ev.Bundle.Items) < 2 {
		t.Fatalf("evidence: %d %s", code, body)
	}
	if strings.Contains(body, fake) || strings.Contains(body, "ada@example.com") {
		t.Errorf("a secret or an address went out: %s", body)
	}
	for _, it := range ev.Bundle.Items {
		if it.Class == "logs" {
			t.Errorf("logs went out although the server allows none for AI: %+v", it)
		}
	}
	// The grant opens nothing else.
	for _, path := range []string{"/api/v1/query?query=up", "/api/v1/incidents/1", "/api/v1/incidents/1/explain", "/api/v1/me"} {
		if code, _ := with(g.Token, "GET", path, ""); code != 401 {
			t.Errorf("grant on %s: %d", path, code)
		}
	}
	if code, _ := with(reader.Token, "GET", "/api/v1/evidence", ""); code != 401 {
		t.Errorf("a read token on the grant's endpoint: %d", code)
	}

	// Used up after twenty reads.
	for i := 2; i <= 20; i++ {
		if code, _ := with(g.Token, "GET", "/api/v1/evidence", ""); code != 200 {
			t.Fatalf("read %d: %d", i, code)
		}
	}
	if code, _ := with(g.Token, "GET", "/api/v1/evidence", ""); code != 429 {
		t.Errorf("read 21: %d", code)
	}
	code, body = admin("GET", "/api/v1/evidence-grants", "")
	if code != 200 || !strings.Contains(body, `"uses":20`) || !strings.Contains(body, `"issuer":"token boop-chat"`) || strings.Contains(body, g.Token) {
		t.Errorf("list: %d %s", code, body)
	}

	// Revoked, a grant reads nothing.
	_, body = admin("POST", "/api/v1/evidence-grants", `{"incident":1,"purpose":"by hand"}`)
	var g2 struct{ ID, Token string }
	json.Unmarshal([]byte(body), &g2)
	if code, _ := with(g2.Token, "GET", "/api/v1/evidence", ""); code != 200 {
		t.Errorf("admin's grant: %d", code)
	}
	if code, _ := admin("DELETE", "/api/v1/evidence-grants/"+g2.ID, ""); code != 204 {
		t.Errorf("revoke: %d", code)
	}
	if code, _ := with(g2.Token, "GET", "/api/v1/evidence", ""); code != 401 {
		t.Errorf("revoked grant: %d", code)
	}
}
