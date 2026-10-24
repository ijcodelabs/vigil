package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
)

// Alerts become incidents on the briefing, a viewer sees them and moves
// their own "seen" marker, only an admin acknowledges.
func TestBriefing(t *testing.T) {
	srv, central := startServer(t, "http://127.0.0.1:1")
	admin := newClient(t, central.URL)
	if code := admin.login("admin", "admin-token-secret"); code != 204 {
		t.Fatalf("login: %d", code)
	}
	if code, body, _ := admin.do("PUT", "/api/v1/users/ada", `{"password":"a-long-password-1","role":"admin"}`); code != 204 {
		t.Fatalf("user: %d %s", code, body)
	}
	// The bootstrap admin is gone once an account exists; ada makes vera.
	ada, vera := newClient(t, central.URL), newClient(t, central.URL)
	ada.login("ada", "a-long-password-1")
	if code, body, _ := ada.do("PUT", "/api/v1/users/vera", `{"password":"a-long-password-1","role":"viewer"}`); code != 204 {
		t.Fatalf("user: %d %s", code, body)
	}
	vera.login("vera", "a-long-password-1")

	ctx, now := context.Background(), time.Now()
	err := srv.SendAlerts(ctx, []alerting.Alert{
		{Labels: map[string]string{"alertname": "HostDown", "agent": "web-1", "severity": "critical"},
			Annotations: map[string]string{"summary": "Agent web-1 stopped reporting"}, StartsAt: now, EndsAt: now.Add(time.Hour)},
		{Labels: map[string]string{"alertname": "ContainerDown", "agent": "web-1", "container": "shop-api", "severity": "critical"},
			StartsAt: now, EndsAt: now.Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.SyncIncidents(ctx, now)

	type incident struct {
		ID       int64
		Kind     string
		Title    string
		State    string
		Children []incident
	}
	var b struct {
		User     string
		Open     []incident
		Since    struct{ Latest, Opened int64 }
		Coverage struct {
			Agents int
			Sync   struct{ Alerts int }
		}
	}
	get := func(c *client) {
		t.Helper()
		code, body, _ := c.do("GET", "/api/v1/briefing", "")
		if code != 200 || json.Unmarshal([]byte(body), &b) != nil {
			t.Fatalf("briefing: %d %s", code, body)
		}
	}
	get(vera)
	if b.User != "vera" || len(b.Open) != 1 || b.Open[0].Kind != "host_down" || len(b.Open[0].Children) != 1 ||
		b.Open[0].Title != "Agent web-1 stopped reporting" || b.Since.Opened != 2 || b.Coverage.Sync.Alerts != 2 {
		t.Fatalf("briefing: %+v", b)
	}
	host := b.Open[0].ID

	// Each person has their own marker.
	if code, body, _ := vera.do("POST", "/api/v1/briefing/seen", `{"event_id":`+jsonInt(b.Since.Latest)+`}`); code != 204 {
		t.Fatalf("seen: %d %s", code, body)
	}
	if get(vera); b.Since.Opened != 0 {
		t.Errorf("vera after marking seen: %+v", b.Since)
	}
	if get(ada); b.Since.Opened != 2 {
		t.Errorf("ada, who has not looked: %+v", b.Since)
	}

	if code, _, _ := vera.do("POST", "/api/v1/incidents/"+jsonInt(host)+"/ack", `{"comment":"on it"}`); code != 403 {
		t.Errorf("a viewer acknowledged: %d", code)
	}
	if code, body, _ := ada.do("POST", "/api/v1/incidents/"+jsonInt(host)+"/ack", `{"comment":"on it"}`); code != 204 {
		t.Fatalf("ack: %d %s", code, body)
	}
	code, body, _ := vera.do("GET", "/api/v1/incidents/"+jsonInt(host), "")
	var one struct {
		Incident struct {
			State   string
			AckBy   string `json:"ack_by"`
			Comment string `json:"ack_comment"`
		} `json:"incident"`
		Events []struct{ Type string }
	}
	if json.Unmarshal([]byte(body), &one); code != 200 || one.Incident.State != "acknowledged" || one.Incident.AckBy != "ada" || one.Incident.Comment != "on it" ||
		len(one.Events) != 2 || one.Events[1].Type != "acknowledged" {
		t.Errorf("incident: %d %s", code, body)
	}
	if code, _, _ := ada.do("POST", "/api/v1/incidents/999/ack", ""); code != 404 {
		t.Errorf("ack of nothing: %d", code)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
