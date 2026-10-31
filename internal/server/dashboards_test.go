package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

func TestDashboards(t *testing.T) {
	cfg := &config.Server{DataDir: t.TempDir(), AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = "http://127.0.0.1:1", "http://127.0.0.1:1"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := func() (*server.Server, *httptest.Server) {
		srv, err := server.New(cfg, quiet)
		if err != nil {
			t.Fatal(err)
		}
		return srv, httptest.NewServer(srv.Handler())
	}
	srv, central := start()
	admin := func(method, path, body string) (int, string) {
		c := &client{t: t, base: central.URL, hc: http.DefaultClient}
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	ids := func() string {
		_, body := admin("GET", "/api/v1/dashboards", "")
		var list []struct{ ID, Title string }
		json.Unmarshal([]byte(body), &list)
		var out []string
		for _, d := range list {
			out = append(out, d.ID)
		}
		return strings.Join(out, ",")
	}

	// A new server comes with the starter dashboards, each of them valid.
	if got := ids(); got != "ai-costs,containers,hosts,logs-and-security,checks,slos,vigil-agents" {
		t.Fatalf("starter dashboards: %q", got)
	}
	code, body := admin("GET", "/api/v1/dashboards/hosts", "")
	var hosts struct {
		Title     string
		Variables []struct{ Name string }
		Panels    []struct{ Type string }
	}
	if err := json.Unmarshal([]byte(body), &hosts); code != 200 || err != nil || hosts.Title != "Hosts" ||
		len(hosts.Panels) < 8 || hosts.Variables[0].Name != "agent" {
		t.Errorf("hosts dashboard: %d %v", code, err)
	}

	good := `{"title":"Shop","variables":[{"name":"env","label":"env"}],"panels":[
	  {"title":"Requests","type":"timeseries","w":8,"h":3,"unit":"ops/s","queries":[{"expr":"sum(rate(http_requests_total{env=~\"$env\"}[5m]))","legend":"all"}]},
	  {"title":"Errors","type":"logs","w":4,"h":3,"limit":20,"queries":[{"expr":"app:shop level:error"}]}]}`
	if code, body := admin("PUT", "/api/v1/dashboards/shop", good); code != http.StatusNoContent {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := admin("GET", "/api/v1/dashboards/shop", ""); code != 200 || !strings.Contains(body, `"id":"shop"`) || !strings.Contains(body, `"unit":"ops/s"`) {
		t.Errorf("read back: %d %s", code, body)
	}
	for want, bad := range map[string]string{
		"title is required":  `{"panels":[]}`,
		"type must be":       `{"title":"x","panels":[{"title":"p","type":"pie","w":6,"h":3,"queries":[{"expr":"up"}]}]}`,
		"w must be":          `{"title":"x","panels":[{"title":"p","type":"stat","w":13,"h":3,"queries":[{"expr":"up"}]}]}`,
		"needs between 1":    `{"title":"x","panels":[{"title":"p","type":"stat","w":3,"h":2,"queries":[]}]}`,
		"a query is empty":   `{"title":"x","panels":[{"title":"p","type":"stat","w":3,"h":2,"queries":[{"expr":" "}]}]}`,
		"unknown unit":       `{"title":"x","panels":[{"title":"p","type":"stat","w":3,"h":2,"unit":"furlongs","queries":[{"expr":"up"}]}]}`,
		"unknown field":      `{"title":"x","panels":[{"title":"p","type":"stat","w":3,"h":2,"colour":"red","queries":[{"expr":"up"}]}]}`,
		"source must be":     `{"title":"x","panels":[{"title":"p","type":"stat","source":"traces","w":3,"h":2,"queries":[{"expr":"up"}]}]}`,
		"must be unique":     `{"title":"x","variables":[{"name":"a","label":"a"},{"name":"a","label":"b"}],"panels":[]}`,
		"unexpected EOF":     `{"title":`,
		"a map query is":     `{"title":"x","panels":[{"title":"m","type":"map","w":12,"h":4,"queries":[{"expr":"up"}]}]}`,
		"nodes need a tier":  `{"title":"x","panels":[{"title":"m","type":"map","w":12,"h":4,"queries":[{"expr":"up","node":"instance"}]}]}`,
		"needs both from":    `{"title":"x","panels":[{"title":"m","type":"map","w":12,"h":4,"queries":[{"expr":"up","from":"job"}]}]}`,
		"are for maps":       `{"title":"x","panels":[{"title":"p","type":"table","w":6,"h":3,"queries":[{"expr":"up","node":"job"}]}]}`,
		"live is for links":  `{"title":"x","panels":[{"title":"m","type":"map","w":12,"h":4,"queries":[{"expr":"up","of":"job","live":true}]}]}`,
		"queries are PromQL": `{"title":"x","panels":[{"title":"m","type":"map","source":"logs","w":12,"h":4,"queries":[{"expr":"x","node":"a","tier":"t"}]}]}`,
	} {
		if code, msg := admin("PUT", "/api/v1/dashboards/bad", bad); code != 400 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s (want %q)", bad, code, msg, want)
		}
	}
	mapPanel := `{"title":"Map","panels":[{"title":"m","type":"map","w":12,"h":5,"columns":["hostname"],"queries":[
	  {"expr":"gw_info","node":"gateway","tier":"Gateways"},
	  {"expr":"agent_connected","node":"exported_agent","tier":"Agents","name":"hostname","group":"hostgroup","warn":0.5,"crit":0},
	  {"expr":"agent_info","from":"gateway","to":"exported_agent"},
	  {"expr":"streams_open","from":"exported_agent","to":"resource","live":true},
	  {"expr":"cert_left","of":"exported_agent","legend":"Certificate left","unit":"seconds","warn":604800,"crit":172800}]}]}`
	if code, body := admin("PUT", "/api/v1/dashboards/map", mapPanel); code != http.StatusNoContent {
		t.Errorf("map: %d %s", code, body)
	}
	admin("DELETE", "/api/v1/dashboards/map", "")
	if code, _ := admin("PUT", "/api/v1/dashboards/Bad_ID", good); code != 400 {
		t.Errorf("bad id accepted: %d", code)
	}
	if code, _ := admin("GET", "/api/v1/dashboards/nope", ""); code != 404 {
		t.Errorf("missing dashboard: %d", code)
	}
	anon := &client{t: t, base: central.URL, hc: http.DefaultClient}
	if code, _, _ := anon.do("GET", "/api/v1/dashboards", ""); code != 401 {
		t.Errorf("anonymous list: %d", code)
	}

	// Deleting a starter dashboard sticks across a restart.
	if code, _ := admin("DELETE", "/api/v1/dashboards/containers", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	central.Close()
	srv.Close()
	srv, central = start()
	defer func() { central.Close(); srv.Close() }()
	if got := ids(); got != "ai-costs,hosts,logs-and-security,shop,checks,slos,vigil-agents" {
		t.Errorf("after restart: %q", got)
	}
}
