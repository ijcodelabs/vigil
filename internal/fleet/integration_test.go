package fleet

import (
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/logs"
)

func TestBuiltinIntegrations(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cat.Names(), ","); got != "ai_module_claude,docker,gpu,haproxy,kubernetes,kubernetes_cluster,nginx,postgres,redis,victoriametrics" {
		t.Fatalf("catalog: %s", got)
	}
	// Each one, switched on with its defaults, gives an agent a valid config.
	for _, name := range cat.Names() {
		p := profile(t, "p", "integrations: ["+name+"]")
		r, err := Resolve([]*Profile{p}, cat, Attributes("x", nil, nil))
		if err != nil || len(r.Integrations) != 1 {
			t.Errorf("%s: %v", name, err)
		}
		if groups, err := cat[name].RuleGroups(nil); err != nil || (len(groups) > 0 && !strings.HasPrefix(groups[0].Name, name+"/")) {
			t.Errorf("%s rules: %v %+v", name, err, groups)
		}
	}
}

func TestIntegrationInProfile(t *testing.T) {
	cat, _ := Builtin()
	base := profile(t, "base", "priority: 0\nconfig:\n  interval: 30s\n")
	db := profile(t, "databases", `
select: ['role="db"']
priority: 20
integrations:
  postgres: {metrics_target: "127.0.0.1:19187", log_path: /srv/pg/log/*.log}
  redis: {}
config:
  scrape:
    static:
      - {job: postgres, interval: 10s}   # adjusts what the integration set up
      - {job: redis, enabled: false}     # this role wants only part of it
`)
	r, err := Resolve([]*Profile{base, db}, cat, Attributes("db-01", map[string]string{"role": "db"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := config.ParseCollection(r.YAML)
	if strings.Join(r.Integrations, ",") != "postgres,redis" || len(c.Scrape.Static) != 1 {
		t.Fatalf("integrations %v, jobs %+v", r.Integrations, c.Scrape.Static)
	}
	pg := c.Scrape.Static[0]
	if pg.Job != "postgres" || pg.Targets[0] != "127.0.0.1:19187" || pg.Interval.Std().Seconds() != 10 || len(pg.Drop) == 0 {
		t.Errorf("postgres job: %+v", pg)
	}
	if len(c.Logs.Files) != 1 || c.Logs.Files[0].Paths[0] != "/srv/pg/log/*.log" || c.Logs.Files[0].Template != "postgres" {
		t.Errorf("log source: %+v", c.Logs.Files)
	}
	origin := map[string]string{}
	for _, o := range r.Origins {
		origin[o.Path] = strings.Join(o.Profiles, ">")
	}
	if origin["scrape.static[postgres]"] != "postgres (integration)>databases" || origin["logs.templates.postgres"] != "postgres (integration)" {
		t.Errorf("origins: %v", origin)
	}

	// Mistakes are named.
	for want, doc := range map[string]string{
		`"mongo", which does not exist`: "integrations: [mongo]",
		`no parameter "port"`:           "integrations: {postgres: {port: '1'}}",
		"list of names, or a map":       "integrations: postgres",
	} {
		p, err := ParseProfile("p", doc)
		if err == nil {
			_, err = Resolve([]*Profile{p}, cat, Attributes("x", nil, nil))
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

func TestDetection(t *testing.T) {
	cat, _ := Builtin()
	inv := &Inventory{Processes: []string{"systemd", "sshd", "postgres", "nginx"},
		Images: []string{"docker.io/library/redis:7.2", "ghcr.io/acme/shop@sha256:abc"}, Ports: []int{22, 5432}}
	if got := strings.Join(cat.Detect(inv), ","); got != "nginx,postgres,redis" {
		t.Errorf("detected: %s", got)
	}
	if got := cat.Detect(&Inventory{Processes: []string{"postgres-exporter"}, Images: []string{"my/nginx-proxy"}}); len(got) != 0 {
		t.Errorf("similar names must not match: %v", got)
	}
	if cat.Detect(nil) != nil {
		t.Error("no inventory, nothing detected")
	}
	// A port alone counts only when the integration lists nothing better.
	portOnly, err := ParseIntegration("memcached", "detect: {port: [11211]}")
	if err != nil || !portOnly.Detected(&Inventory{Ports: []int{11211}}) || cat["postgres"].Detected(&Inventory{Ports: []int{5432}}) {
		t.Errorf("port detection: %v", err)
	}
}

func TestParseIntegrationErrors(t *testing.T) {
	for want, doc := range map[string]string{
		"not declared under params": "config: {scrape: {static: [{job: x, targets: ['${target}']}]}}",
		"field statik":              "config: {scrape: {statik: []}}",
		"rules:":                    "rules: [{name: g, rules: [{alert: A}]}]",
		"field detekt":              "detekt: {}",
	} {
		if _, err := ParseIntegration("x", doc); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
	if _, err := ParseIntegration("Bad-Name", ""); err == nil {
		t.Error("bad name accepted")
	}
}

// The built-in log templates are checked against lines as the software
// really writes them.
func TestBuiltinLogTemplates(t *testing.T) {
	cat, _ := Builtin()
	parse := func(integration, template, line string) (string, map[string]string, map[string]string, bool) {
		t.Helper()
		r, err := Resolve([]*Profile{profile(t, "p", "integrations: ["+integration+"]")}, cat, Attributes("x", nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		c, _ := config.ParseCollection(r.YAML)
		tm, err := logs.Compile(template, c.Logs.Templates[template])
		if err != nil {
			t.Fatal(err)
		}
		msg, ts, labels, fields, ok := tm.Process(line)
		return msg, labels, fields, ok && !ts.IsZero() == (template != "postgres")
	}
	_, labels, fields, ok := parse("nginx", "nginx_access",
		`203.0.113.7 - alice [10/Oct/2026:13:55:36 +0000] "GET /api/items?page=2 HTTP/1.1" 502 157 "https://shop.example/" "Mozilla/5.0 (X11; Linux x86_64)"`)
	if !ok || labels["status"] != "502" || fields["method"] != "GET" || fields["path"] != "/api/items?page=2" || fields["client"] != "203.0.113.7" || fields["agent_string"] == "" {
		t.Errorf("nginx access: labels=%v fields=%v", labels, fields)
	}
	msg, labels, _, ok := parse("nginx", "nginx_error",
		`2026/10/10 13:55:36 [error] 31#31: *7 connect() failed (111: Connection refused) while connecting to upstream, client: 203.0.113.7`)
	if !ok || labels["level"] != "error" || !strings.HasPrefix(msg, "31#31: *7 connect() failed") {
		t.Errorf("nginx error: %q %v", msg, labels)
	}
	msg, labels, _, _ = parse("postgres", "postgres",
		"2026-10-10 13:55:36.123 UTC [4211] shop@orders ERROR:  duplicate key value violates unique constraint \"orders_pkey\"\nDETAIL:  Key (id)=(42) already exists.")
	if labels["level"] != "ERROR" || !strings.HasPrefix(msg, "duplicate key value") || !strings.Contains(msg, "DETAIL:") {
		t.Errorf("postgres: %q %v", msg, labels)
	}
	if msg, labels, _, _ = parse("postgres", "postgres", "2026-10-10 13:55:37.001 UTC [1] LOG:  checkpoint starting: time"); labels["level"] != "LOG" || msg != "checkpoint starting: time" {
		t.Errorf("postgres without user@db: %q %v", msg, labels)
	}
}

// The HAProxy template against lines as a real HAProxy 3.0 writes them.
func TestHaproxyLogTemplate(t *testing.T) {
	cat, _ := Builtin()
	r, err := Resolve([]*Profile{profile(t, "p", "integrations: {haproxy: {log_container: edge-proxy}}")}, cat, Attributes("x", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseCollection(r.YAML)
	if err != nil {
		t.Fatal(err)
	}
	// The parameter chooses which containers the template is for.
	if c.Docker.ContainerTemplates["edge-proxy"] != "haproxy_http" || len(c.Docker.ContainerTemplates) != 1 {
		t.Errorf("container templates: %v", c.Docker.ContainerTemplates)
	}
	if err := logs.Validate(c); err != nil {
		t.Fatal(err)
	}
	tm, err := logs.Compile("haproxy_http", c.Logs.Templates["haproxy_http"])
	if err != nil {
		t.Fatal(err)
	}
	for line, want := range map[string]map[string]string{
		`203.0.113.9:46907 [10/Oct/2026:16:06:40.992] https-in~ web_backend/web_server 6/0/1/1/8 200 22873 - - ---- 3/1/0/0/0 0/0 "GET / HTTP/1.1"`: {
			"client": "203.0.113.9", "frontend": "https-in~", "backend": "web_backend", "server": "web_server",
			"response_ms": "1", "total_ms": "8", "status": "200", "bytes": "22873", "termination": "----", "method": "GET", "path": "/"},
		`203.0.113.9:9721 [10/Oct/2026:16:06:40.863] https-in~ https-in/<NOSRV> 6/-1/-1/-1/6 301 94 - - LR-- 3/1/0/0/0 0/0 "GET / HTTP/1.1"`: {
			"backend": "https-in", "server": "<NOSRV>", "response_ms": "-1", "status": "301", "termination": "LR--"},
		`10.0.0.7:43283 [10/Oct/2026:16:06:48.543] https-in~ forum_backend/forum_server 5/0/0/26/+37 502 +492 - - SH-- 3/1/0/0/0 0/0 "POST /message-bus/34a13c/poll?dlp=t HTTP/1.1"`: {
			"backend": "forum_backend", "total_ms": "37", "status": "502", "bytes": "492", "method": "POST", "path": "/message-bus/34a13c/poll"},
		`[2001:db8::1]:5120 [10/Oct/2026:16:06:48.543] https-in~ api_backend/api1 0/0/0/3/3 204 120 - - ---- 1/1/0/0/0 0/0 "OPTIONS /v1/x HTTP/2.0"`: {
			"client": "[2001:db8::1]", "status": "204", "method": "OPTIONS"},
	} {
		_, _, labels, fields, ok := tm.Process(line)
		if !ok || labels["app"] != "haproxy" {
			t.Errorf("%s: ok=%v labels=%v", line, ok, labels)
		}
		for k, v := range want {
			if fields[k] != v {
				t.Errorf("%s:\n  %s = %q, want %q (all: %v)", line, k, fields[k], v, fields)
			}
		}
	}
	// Anything else HAProxy prints is kept as it is, still marked as HAProxy's.
	msg, _, labels, fields, ok := tm.Process(`[NOTICE]   (1) : New worker (8) forked`)
	if !ok || msg != `[NOTICE]   (1) : New worker (8) forked` || labels["app"] != "haproxy" || len(fields) != 0 {
		t.Errorf("notice line: %q %v %v", msg, labels, fields)
	}
}

func TestQueries(t *testing.T) {
	list, err := ParseQueries("queries:\n  - {name: ' Load ', query: host_load1}\n  - {name: Load, type: logs, query: 'load | stats count() as n'}\n")
	if err != nil || len(list) != 2 || list[0].Name != "Load" || list[0].Type != QueryMetrics || list[1].Type != QueryLogs {
		t.Fatalf("%+v %v", list, err)
	}
	if list, err := ParseQueries("# nothing yet\n"); err != nil || len(list) != 0 {
		t.Errorf("an empty file: %+v %v", list, err)
	}
	for text, want := range map[string]string{
		"queries: [{name: a, query: up}, {name: a, query: down}]": "defined twice",
		"queries: [{name: a, type: traces, query: up}]":           "type must be",
		"queries: [{name: a}]":                                    "empty",
		"queries: [{query: up}]":                                  "needs a name",
		"queries: [{name: a, query: up, unit: bytes}]":            "field unit not found",
		"panels: []": "field panels not found",
	} {
		if _, err := ParseQueries(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error with %q", text, err, want)
		}
	}
	// Integrations bring queries; a bad one refuses the whole integration.
	cat, _ := Builtin()
	for _, name := range cat.Names() {
		if len(cat[name].Queries) == 0 {
			t.Errorf("built-in integration %s has no queries for Explore", name)
		}
	}
	if _, err := ParseIntegration("shop", "queries: [{name: Orders, type: sql, query: x}]"); err == nil || !strings.Contains(err.Error(), "queries: ") {
		t.Errorf("an integration with a bad query: %v", err)
	}
}

// Parameters reach the rules as well as the config: GpuHot uses the
// default 88, or the value a profile gives.
func TestRuleParams(t *testing.T) {
	cat, _ := Builtin()
	expr := func(params map[string]string) string {
		groups, err := cat["gpu"].RuleGroups(params)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			for _, r := range g.Rules {
				if r.Alert == "GpuHot" {
					return r.Expr
				}
			}
		}
		t.Fatal("no GpuHot rule")
		return ""
	}
	if e := expr(nil); e != "gpu_temperature_celsius > 88" {
		t.Errorf("defaults: %q", e)
	}
	if e := expr(map[string]string{"hot_celsius": "92"}); e != "gpu_temperature_celsius > 92" {
		t.Errorf("given: %q", e)
	}
	for _, name := range cat.Names() {
		groups, _ := cat[name].RuleGroups(nil)
		for _, g := range groups {
			for _, r := range g.Rules {
				if strings.Contains(r.Expr, "${") || strings.Contains(r.Annotations["summary"], "${") {
					t.Errorf("%s %s: a parameter is left in %q", name, r.Alert, r.Expr)
				}
			}
		}
	}
}
