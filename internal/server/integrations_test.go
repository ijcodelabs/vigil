package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIntegrationsAndDetection(t *testing.T) {
	_, central := startServer(t, "http://127.0.0.1:1")
	admin := func(method, path, body string) (int, string) {
		c := &client{t: t, base: central.URL, hc: http.DefaultClient}
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	poll := func(id, report string) (int, string, http.Header) {
		c := &client{t: t, base: central.URL, hc: http.DefaultClient}
		return c.do("POST", "/api/v1/agent/config", report, "Authorization", "Bearer ingest", "X-Vigil-Agent", id)
	}

	// The first poll carries no inventory; the server asks for it.
	if _, _, h := poll("db-01", `{"labels":{"env":"prod"}}`); h.Get("X-Vigil-Want-Inventory") == "" {
		t.Error("server did not ask for the inventory it lacks")
	}
	_, _, h := poll("db-01", `{"labels":{"env":"prod"},"inventory":{"processes":["systemd","postgres","nginx"],"ports":[22,5432],"images":[]}}`)
	if h.Get("X-Vigil-Want-Inventory") != "" {
		t.Error("server asked again for an inventory it just got")
	}
	poll("web-01", `{"labels":{"env":"prod"},"inventory":{"processes":["nginx"],"ports":[80],"images":["docker.io/library/redis:7"]}}`)
	poll("app-01", `{"labels":{"env":"prod"},"inventory":{"processes":["java"],"ports":[8080],"images":[]}}`)

	catalog := func() map[string]map[string]any {
		_, body := admin("GET", "/api/v1/fleet/integrations", "")
		var list []map[string]any
		json.Unmarshal([]byte(body), &list)
		out := map[string]map[string]any{}
		for _, it := range list {
			out[it["name"].(string)] = it
		}
		return out
	}
	cat := catalog()
	if len(cat) != 9 || cat["nginx"]["detected"] != 2.0 || cat["nginx"]["unmonitored"] != 2.0 || cat["postgres"]["detected"] != 1.0 ||
		cat["redis"]["detected"] != 1.0 || cat["docker"]["detected"] != 0.0 || cat["nginx"]["builtin"] != true {
		t.Fatalf("catalog: nginx=%v postgres=%v redis=%v", cat["nginx"], cat["postgres"], cat["redis"])
	}
	if list, _ := cat["nginx"]["unmonitored_agents"].([]any); len(list) != 2 || list[0] != "db-01" {
		t.Errorf("unmonitored agents for nginx: %v", cat["nginx"]["unmonitored_agents"])
	}

	// Mistakes in a profile's integrations are refused even though nothing matches yet.
	for doc, want := range map[string]string{
		"integrations: [mongo]":                    `"mongo" does not exist`,
		"integrations: {postgres: {port: '5433'}}": `no parameter "port"`,
	} {
		if code, msg := admin("PUT", "/api/v1/fleet/profiles/bad", doc); code != 400 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s", doc, code, msg)
		}
	}

	// "Wherever nginx runs, collect nginx": the selector uses what was detected.
	if code, msg := admin("PUT", "/api/v1/fleet/profiles/nginx", "select: ['detected_nginx=\"true\"']\npriority: 50\nintegrations:\n  nginx: {access_log: /srv/log/access.log}\n"); code != 200 {
		t.Fatalf("save profile: %d %s", code, msg)
	}
	code, cfg, _ := poll("web-01", `{"labels":{"env":"prod"}}`)
	if code != 200 || !strings.Contains(cfg, "job: nginx") || !strings.Contains(cfg, "/srv/log/access.log") || !strings.Contains(cfg, "nginx_error") {
		t.Errorf("web-01 config: %d\n%s", code, cfg)
	}
	if code, _, _ := poll("app-01", `{"labels":{"env":"prod"}}`); code != 204 {
		t.Errorf("app-01 runs no nginx and should get nothing: %d", code)
	}
	cat = catalog()
	if cat["nginx"]["enabled"] != 2.0 || cat["nginx"]["unmonitored"] != 0.0 || cat["postgres"]["unmonitored"] != 1.0 {
		t.Errorf("after enabling: nginx=%v", cat["nginx"])
	}
	if p, _ := cat["nginx"]["profiles"].([]any); len(p) != 1 || p[0] != "nginx" {
		t.Errorf("profiles using nginx: %v", cat["nginx"]["profiles"])
	}
	// nginx appearing on another host is picked up without touching the profile.
	poll("app-01", `{"labels":{"env":"prod"},"inventory":{"processes":["java","nginx"],"ports":[8080,80],"images":[]}}`)
	if code, cfg, _ := poll("app-01", `{"labels":{"env":"prod"}}`); code != 200 || !strings.Contains(cfg, "job: nginx") {
		t.Errorf("app-01 after nginx appeared: %d", code)
	}

	// Its dashboard is installed once, and its rules run next to the user's.
	if code, body := admin("GET", "/api/v1/dashboards/nginx", ""); code != 200 || !strings.Contains(body, `"title":"nginx"`) {
		t.Errorf("integration dashboard: %d %s", code, body)
	}
	if code, msg := admin("PUT", "/api/v1/rules/config", "groups: []\n"); code != http.StatusNoContent {
		t.Fatalf("start rules: %d %s", code, msg)
	}
	if _, body := admin("GET", "/api/v1/rules", ""); !strings.Contains(body, `"name":"nginx/alerts"`) || !strings.Contains(body, `"name":"nginx/logs"`) || strings.Contains(body, "postgres/") {
		t.Errorf("rules in effect: %s", body)
	}
	admin("DELETE", "/api/v1/dashboards/nginx", "")

	// A server's own integration, and a customised built-in one.
	custom := "title: Shop\ndetect: {process: [java]}\nparams:\n  target: {default: 'localhost:8080'}\nconfig:\n  scrape:\n    static:\n      - {job: shop, targets: ['${target}'], path: /actuator/prometheus}\n"
	if code, msg := admin("PUT", "/api/v1/fleet/integrations/shop", custom); code != http.StatusNoContent {
		t.Fatalf("custom integration: %d %s", code, msg)
	}
	if cat = catalog(); cat["shop"]["detected"] != 1.0 || cat["shop"]["builtin"] != false {
		t.Errorf("custom integration in catalog: %v", cat["shop"])
	}
	for doc, want := range map[string]string{
		"config: {scrape: {static: [{job: x, targets: ['${nope}']}]}}":                              "not declared under params",
		"dashboard: {title: X, panels: [{title: p, type: pie, w: 3, h: 2, queries: [{expr: up}]}]}": "dashboard: panel 1",
		"rules: [{name: g, rules: [{alert: A}]}]":                                                   "expr is required",
	} {
		if code, msg := admin("PUT", "/api/v1/fleet/integrations/broken", doc); code != 400 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s", doc, code, msg)
		}
	}
	// Changing nginx so that the profile's parameter no longer exists would break two agents.
	if code, msg := admin("PUT", "/api/v1/fleet/integrations/nginx", "title: nginx\nconfig: {}\n"); code != 400 || !strings.Contains(msg, `no parameter "access_log"`) {
		t.Errorf("breaking change to an integration in use: %d %s", code, msg)
	}
	if code, msg := admin("DELETE", "/api/v1/fleet/integrations/redis", ""); code != 400 || !strings.Contains(msg, "built in") {
		t.Errorf("deleting a built-in: %d %s", code, msg)
	}
	admin("PUT", "/api/v1/fleet/profiles/shop", "select: ['detected_shop=\"true\"']\nintegrations: [shop]\n")
	if code, msg := admin("DELETE", "/api/v1/fleet/integrations/shop", ""); code != 400 || !strings.Contains(msg, "profile shop") {
		t.Errorf("deleting an integration a profile uses: %d %s", code, msg)
	}
	admin("DELETE", "/api/v1/fleet/profiles/shop", "")
	if code, _ := admin("DELETE", "/api/v1/fleet/integrations/shop", ""); code != http.StatusNoContent {
		t.Errorf("delete unused custom integration: %d", code)
	}

	// With the profile gone, the rules go too; the deleted dashboard does not come back.
	admin("DELETE", "/api/v1/fleet/profiles/nginx", "")
	if _, body := admin("GET", "/api/v1/rules", ""); strings.Contains(body, "nginx/") {
		t.Errorf("rules of an unused integration still run: %s", body)
	}
	admin("PUT", "/api/v1/fleet/profiles/nginx", "integrations: [nginx]\n")
	if code, _ := admin("GET", "/api/v1/dashboards/nginx", ""); code != 404 {
		t.Errorf("a dashboard the user deleted was installed again: %d", code)
	}
}
