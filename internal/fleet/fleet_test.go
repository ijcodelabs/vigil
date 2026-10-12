package fleet

import (
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
)

func profile(t *testing.T, name, text string) *Profile {
	t.Helper()
	p, err := ParseProfile(name, text)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

const (
	base = `
priority: 0
config:
  interval: 30s
  labels: {managed: "yes"}
  scrape:
    static:
      - {job: node, targets: ["localhost:9100"], interval: 60s}
      - {job: app, targets: ["localhost:8080"]}
  logs:
    templates:
      plain: {}
    files:
      - {name: syslog, paths: [/var/log/syslog], template: plain}
  security:
    watch_paths: [/etc/shadow]
    rules:
      - {name: shadow_read, when: 'type == "open" && path.startsWith("/etc/shadow")'}
`
	prod = `
select: ['env="prod"']
priority: 10
config:
  interval: 15s
  labels: {tier: gold}
  scrape:
    static:
      - {job: node, interval: 15s, drop: ['^go_']}
  security:
    enabled: true
`
	databases = `
select: ['role=~"db|cache"', 'os="linux"']
priority: 20
config:
  scrape:
    static:
      - {job: postgres, targets: ["localhost:9187"], basic_auth: {username: mon, password: pw}}
      - {job: app, enabled: false}
  logs:
    templates:
      pg: {multiline: {start: '^\d{4}-'}}
    files:
      - {name: postgres, paths: [/var/log/postgresql/*.log], template: pg}
      - {name: syslog, enabled: false}
  security:
    watch_paths: [/etc/shadow, /var/lib/postgresql/]
    rules:
      - {name: shadow_read, severity: critical, capture: true}
`
	oneHost = `
select: ['agent="db-07"']
priority: 1000
config:
  interval: 5s
`
)

func TestResolveLayers(t *testing.T) {
	all := []*Profile{profile(t, "one-host", oneHost), profile(t, "databases", databases), profile(t, "prod", prod), profile(t, "base", base)}

	// A prod database: base, prod and databases apply, in that order.
	attrs := Attributes("db-01", map[string]string{"env": "prod", "role": "db"}, map[string]string{"os": "linux"})
	r, err := Resolve(all, nil, attrs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Profiles, ","); got != "base,prod,databases" {
		t.Fatalf("profiles: %s", got)
	}
	c, err := config.ParseCollection(r.YAML)
	if err != nil {
		t.Fatalf("resolved config does not parse: %v\n%s", err, r.YAML)
	}
	if c.Interval.Std().Seconds() != 15 || c.Labels["managed"] != "yes" || c.Labels["tier"] != "gold" || !c.Security.Enabled {
		t.Errorf("scalars and maps: interval=%v labels=%v", c.Interval.Std(), c.Labels)
	}
	jobs := map[string]config.StaticJob{}
	for _, j := range c.Scrape.Static {
		jobs[j.Job] = j
	}
	node := jobs["node"]
	if len(jobs) != 2 || node.Interval.Std().Seconds() != 15 || node.Targets[0] != "localhost:9100" || node.Drop[0] != "^go_" {
		t.Errorf("jobs: %+v", jobs) // node overridden field by field, app switched off, postgres added
	}
	if jobs["postgres"].BasicAuth == nil || jobs["postgres"].BasicAuth.Username != "mon" {
		t.Errorf("postgres job: %+v", jobs["postgres"])
	}
	if len(c.Logs.Files) != 1 || c.Logs.Files[0].Name != "postgres" || len(c.Logs.Templates) != 2 {
		t.Errorf("log sources: %+v templates: %v", c.Logs.Files, c.Logs.Templates)
	}
	rule := c.Security.Rules[0]
	if len(c.Security.Rules) != 1 || rule.Severity != "critical" || !rule.Capture || !strings.Contains(rule.When, "/etc/shadow") {
		t.Errorf("rule merged by name: %+v", rule)
	}
	if strings.Join(c.Security.WatchPaths, ",") != "/etc/shadow,/var/lib/postgresql/" {
		t.Errorf("plain lists are replaced by the later profile: %v", c.Security.WatchPaths)
	}
	if strings.Contains(r.YAML, "enabled: false") || r.Hash != config.Hash(r.YAML) {
		t.Errorf("resolved text:\n%s", r.YAML)
	}

	origin := map[string]string{}
	for _, o := range r.Origins {
		origin[o.Path] = strings.Join(o.Profiles, ">")
	}
	for path, want := range map[string]string{
		"interval": "base>prod", "labels": "base>prod", "scrape.static[node]": "base>prod", "scrape.static[app]": "base>databases",
		"scrape.static[postgres]": "databases", "logs.templates.pg": "databases", "security.rules[shadow_read]": "base>databases",
		"security.enabled": "prod", "security.watch_paths": "base>databases",
	} {
		if origin[path] != want {
			t.Errorf("origin of %s: %q, want %q (all: %v)", path, origin[path], want, origin)
		}
	}

	// The same input always gives the same text, so the hash is stable.
	again, _ := Resolve(all, nil, attrs)
	if again.YAML != r.YAML {
		t.Error("resolving twice gave different text")
	}

	// A staging web server only gets the base.
	r, _ = Resolve(all, nil, Attributes("web-01", map[string]string{"env": "staging", "role": "web"}, map[string]string{"os": "linux"}))
	if strings.Join(r.Profiles, ",") != "base" {
		t.Errorf("staging web: %v", r.Profiles)
	}
	// The single-host profile wins over everything for that host.
	r, _ = Resolve(all, nil, Attributes("db-07", map[string]string{"env": "prod", "role": "db"}, map[string]string{"os": "linux"}))
	if c, _ := config.ParseCollection(r.YAML); strings.Join(r.Profiles, ",") != "base,prod,databases,one-host" || c.Interval.Std().Seconds() != 5 {
		t.Errorf("db-07: %v", r.Profiles)
	}
	// A label cannot stand in for a fact: os comes from the agent itself.
	r, _ = Resolve(all, nil, Attributes("mac-01", map[string]string{"role": "db", "os": "linux"}, map[string]string{"os": "darwin"}))
	if strings.Contains(strings.Join(r.Profiles, ","), "databases") {
		t.Errorf("a label overrode a fact: %v", r.Profiles)
	}
	// No profile at all: the agent keeps its own file.
	r, err = Resolve([]*Profile{profile(t, "prod", prod)}, nil, Attributes("x", nil, nil))
	if err != nil || r.YAML != "" || len(r.Profiles) != 0 {
		t.Errorf("no match: %+v %v", r, err)
	}
}

func TestResolveRejectsBrokenCombination(t *testing.T) {
	// Each profile is fine alone; together the log source names a template nobody defines.
	a := profile(t, "a", "config:\n  logs:\n    files:\n      - {name: app, paths: [/var/log/app.log], template: missing}\n")
	if _, err := Resolve([]*Profile{a}, nil, Attributes("x", nil, nil)); err == nil || !strings.Contains(err.Error(), `unknown log template "missing"`) || !strings.Contains(err.Error(), "profiles a") {
		t.Errorf("got %v", err)
	}
	// Switching off a job that needs nothing else is fine even if no profile defines it.
	b := profile(t, "b", "config:\n  scrape:\n    static:\n      - {job: ghost, enabled: false}\n")
	if r, err := Resolve([]*Profile{b}, nil, Attributes("x", nil, nil)); err != nil || strings.Contains(r.YAML, "ghost") {
		t.Errorf("disable of unknown job: %v\n%s", err, r.YAML)
	}
	// A job with no targets anywhere is refused.
	c := profile(t, "c", "config:\n  scrape:\n    static:\n      - {job: half, interval: 10s}\n")
	if _, err := Resolve([]*Profile{c}, nil, Attributes("x", nil, nil)); err == nil || !strings.Contains(err.Error(), "targets is empty") {
		t.Errorf("got %v", err)
	}
}

func TestParseProfileErrors(t *testing.T) {
	for want, text := range map[string]string{
		"select":            "select: ['env prod']",
		"field statik":      "config:\n  scrape:\n    statik: []",
		"field selector":    "selector: ['a=\"b\"']",
		"it is being saved": "name: other",
	} {
		if _, err := ParseProfile("p", text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", text, err, want)
		}
	}
	if _, err := ParseProfile("Bad Name", ""); err == nil {
		t.Error("bad name accepted")
	}
	// The server address is not something a profile may set.
	if _, err := ParseProfile("p", "config:\n  server: {url: http://evil}"); err == nil {
		t.Error("profile setting the server accepted")
	}
	// An empty profile is valid and selects everyone.
	p := profile(t, "empty", "")
	if !p.Matches(map[string]string{"anything": "x"}) {
		t.Error("empty selector should match")
	}
	// Selectors: negation and regular expressions; a missing label is empty.
	// As in Prometheus, an expression has to match the whole value.
	p = profile(t, "sel", `select: ['env!="dev"', 'kernel=~"(5|6|7)\\..*"', 'zone=""']`)
	if !p.Matches(map[string]string{"env": "prod", "kernel": "6.8.0"}) || p.Matches(map[string]string{"env": "dev", "kernel": "6.8.0"}) ||
		p.Matches(map[string]string{"env": "prod", "kernel": "4.18.0"}) || p.Matches(map[string]string{"env": "prod", "kernel": "6.8", "zone": "a"}) {
		t.Error("selector matching")
	}
}

// Probes and process groups are top-level lists that merge by name too.
func TestResolveProbesAndProcesses(t *testing.T) {
	base := profile(t, "base", `
config:
  probes:
    - {name: site, http: "https://example.com/", interval: 60s}
    - {name: db, tcp: "db.internal:5432"}
  processes:
    - {name: sshd}
  systemd: {enabled: true, units: [sshd.service]}
  limits: {max_series: 5000}
`)
	edge := profile(t, "edge", `
select: ['role="edge"']
priority: 10
config:
  probes:
    - {name: site, interval: 10s}
    - {name: db, enabled: false}
  processes:
    - {name: nginx}
  otlp: {enabled: true}
`)
	r, err := Resolve([]*Profile{base, edge}, nil, Attributes("e1", map[string]string{"role": "edge"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := config.ParseCollection(r.YAML)
	if len(c.Probes) != 1 || c.Probes[0].Name != "site" || c.Probes[0].Interval.Std().Seconds() != 10 || c.Probes[0].HTTP != "https://example.com/" {
		t.Errorf("probes: %+v", c.Probes)
	}
	if len(c.Processes) != 2 || !c.Systemd.Enabled || !c.OTLP.Enabled || c.OTLP.Listen != "127.0.0.1:4318" || c.Limits.MaxSeries != 5000 {
		t.Errorf("processes %+v systemd %+v otlp %+v limits %+v", c.Processes, c.Systemd, c.OTLP, c.Limits)
	}
	for want, doc := range map[string]string{
		"exactly one of http, tcp, tls and ping": "config: {probes: [{name: x}]}",
		"must be a URL":                          "config: {probes: [{name: x, http: example.com}]}",
		"match or cmdline":                       "config: {processes: [{name: x, match: a, cmdline: b}]}",
	} {
		if _, err := Resolve([]*Profile{profile(t, "p", doc)}, nil, Attributes("x", nil, nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}
