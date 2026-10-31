package slo

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndQueries(t *testing.T) {
	s, err := Parse("shop-api", `
objective: 99.9
indicator:
  span:
    service: shop-api
    span_name: ["GET /cart", "POST /checkout?x"]
    latency_below: 500ms
labels: {team: shop}
`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "shop-api" || s.Window != "30d" || s.Service != "shop-api" || s.Indicator.Span.Kind != "server" {
		t.Fatalf("defaults not filled: %+v", s)
	}
	good, total := s.Queries()
	wantTotal := `sum(increase(traces_spans_total{service="shop-api", kind="server", span_name=~"GET /cart|POST /checkout\\?x"}[1m]))`
	if total != wantTotal {
		t.Errorf("total:\n got %s\nwant %s", total, wantTotal)
	}
	if !strings.Contains(good, `traces_span_duration_seconds_bucket{service="shop-api", kind="server", span_name=~"GET /cart|POST /checkout\\?x", status="ok", le="0.5"}`) ||
		!strings.HasSuffix(good, "and on() "+total) {
		t.Errorf("good: %s", good)
	}
	g := s.RuleGroup()
	if g.Name != "slo:shop-api" || g.Labels["slo"] != "shop-api" || g.Labels["team"] != "shop" || len(g.Rules) != 3 {
		t.Fatalf("group: %+v", g)
	}
	if g.Rules[2].Expr != "vector(0.999)" {
		t.Errorf("objective: %s", g.Rules[2].Expr)
	}
}

func TestProbeAndRatioQueries(t *testing.T) {
	p, err := Parse("", `
name: home
objective: 99.5
window: calendar_month
indicator: {probe: {name: shop-home, agents: [edge-1, edge-2]}}
`)
	if err != nil {
		t.Fatal(err)
	}
	good, total := p.Queries()
	if total != `sum(count_over_time(probe_success{probe="shop-home", agent=~"edge-1|edge-2"}[1m]))` {
		t.Errorf("probe total: %s", total)
	}
	if !strings.HasPrefix(good, `((sum(sum_over_time(probe_success{probe="shop-home", agent=~"edge-1|edge-2"}[1m]))) or vector(0))`) {
		t.Errorf("probe good: %s", good)
	}

	r, err := Parse("lb", `
objective: 99
indicator:
  ratio:
    bad: haproxy_backend_http_responses_total{proxy="shop", code="5xx"}
    total: haproxy_backend_http_responses_total{proxy="shop"}
`)
	if err != nil {
		t.Fatal(err)
	}
	good, total = r.Queries()
	want := `((sum(increase(haproxy_backend_http_responses_total{proxy="shop"}[1m])) - (sum(increase(haproxy_backend_http_responses_total{proxy="shop", code="5xx"}[1m])) or vector(0))) or vector(0)) and on() ` + total
	if good != want {
		t.Errorf("ratio good:\n got %s\nwant %s", good, want)
	}
}

func TestRefused(t *testing.T) {
	for _, c := range []struct{ doc, want string }{
		{"objective: 99.9\nindicator: {probe: {name: x}}\nwindw: 7d", "field windw not found"},
		{"name: other\nobjective: 99\nindicator: {probe: {name: x}}", "being saved as"},
		{"objective: 100\nindicator: {probe: {name: x}}", "above 50 and below 100"},
		{"objective: 99\nwindow: 1y\nindicator: {probe: {name: x}}", "window"},
		{"objective: 99\nindicator: {}", "exactly one"},
		{"objective: 99\nindicator: {probe: {name: x}, span: {service: y}}", "exactly one"},
		{"objective: 99\nindicator: {span: {service: y, latency_below: 300ms}}", "nearest are 250ms and 500ms"},
		{"objective: 99\nindicator: {span: {service: y, latency_below: 1m}}", "largest"},
		{"objective: 99\nindicator: {span: {service: y, kind: weird}}", "kind"},
		{"objective: 99\nindicator: {ratio: {total: 'sum(x)', bad: y}}", "not a series selector"},
		{"objective: 99\nindicator: {ratio: {total: 'x[5m]', bad: y}}", "not a series selector"},
		{"objective: 99\nindicator: {ratio: {total: x}}", "exactly one of good and bad"},
		{"objective: 99\nindicator: {probe: {name: x}}\nlabels: {slo: other}", "set by the SLO itself"},
	} {
		_, err := Parse("a", c.doc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want an error with %q", c.doc, err, c.want)
		}
	}
	if _, err := Parse("Bad_Name", "objective: 99\nindicator: {probe: {name: x}}"); err == nil {
		t.Error("a name with capitals and underscores was accepted")
	}
}

func TestAlerts(t *testing.T) {
	s, err := Parse("api", `
objective: 99.9
runbook_url: https://wiki.example.com/api
indicator: {span: {service: api}}
`)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := s.AlertGroup()
	if !ok || g.Name != "slo:api:alerts" || g.Delay != 0 || g.Labels["slo"] != "api" || len(g.Rules) != 3 {
		t.Fatalf("alert group: %+v", g)
	}
	fast, slow, gone := g.Rules[0], g.Rules[1], g.Rules[2]
	if fast.Alert != "SLOBurnFast" || fast.Labels["severity"] != "critical" || slow.Alert != "SLOBurnSlow" || gone.Alert != "SLOBudgetExhausted" {
		t.Fatalf("alerts: %s %s %s", fast.Alert, slow.Alert, gone.Alert)
	}
	// The usual rates for 30 days, both windows, and the event minimum
	// (10 an hour by default) over the long window.
	// The usual rates for 30 days, both windows ending two minutes back, the
	// event minimum (10 an hour by default) and a full long window.
	for _, want := range []string{
		`sum(sum_over_time(slo_good_per_minute{slo="api"}[1h] offset 2m)) / sum(sum_over_time(slo_total_per_minute{slo="api"}[1h] offset 2m))) / 0.001) > 14.4`,
		`[5m] offset 2m))) / 0.001) > 14.4`,
		`(time() - tfirst_over_time(slo_total_per_minute{slo="api"}[1h])) >= 2700`,
		`sum(sum_over_time(slo_total_per_minute{slo="api"}[1h] offset 2m)) >= 10`,
		`[30m] offset 2m))) / 0.001) > 6`,
		`(time() - tfirst_over_time(slo_total_per_minute{slo="api"}[6h])) >= 16200`,
		`sum(sum_over_time(slo_total_per_minute{slo="api"}[6h] offset 2m)) >= 60`,
	} {
		if !strings.Contains(fast.Expr, want) {
			t.Errorf("SLOBurnFast lacks %s:\n%s", want, fast.Expr)
		}
	}
	for _, want := range []string{"[1d]", "> 3 and", "[2h]", "[3d]", "> 1 and", "[6h]", ">= 240", ">= 720"} {
		if !strings.Contains(slow.Expr, want) {
			t.Errorf("SLOBurnSlow lacks %s:\n%s", want, slow.Expr)
		}
	}
	if !strings.Contains(gone.Expr, "[30d] offset 2m") || !strings.Contains(gone.Expr, ">= 0.001 and on()") || !strings.HasSuffix(gone.Expr, "[30d])) >= 648000") {
		t.Errorf("SLOBudgetExhausted: %s", gone.Expr)
	}
	if fast.Annotations["runbook_url"] != "https://wiki.example.com/api" || !strings.Contains(fast.Annotations["summary"], `{{ printf "%.1f" $value }}`) {
		t.Errorf("annotations: %v", fast.Annotations)
	}
	if len(s.Warnings()) != 0 {
		t.Errorf("warnings: %v", s.Warnings())
	}

	// A 7-day window spends the same share of its budget, faster; a probe
	// SLO has no event minimum; switches leave alerts out.
	p, _ := Parse("home", "objective: 99\nwindow: 7d\nindicator: {probe: {name: x}}\nalerting: {ticket: false}")
	g, _ = p.AlertGroup()
	if len(g.Rules) != 1 || g.Rules[0].Alert != "SLOBurnFast" || !strings.Contains(g.Rules[0].Expr, "> 3.36 and") ||
		!strings.Contains(g.Rules[0].Expr, "> 1.4 and") || strings.Contains(g.Rules[0].Expr, "offset 2m)) >=") {
		t.Errorf("7d probe SLO: %+v", g.Rules)
	}
	off, _ := Parse("off", "objective: 99\nindicator: {probe: {name: x}}\nalerting: {page: false, ticket: false}")
	if _, ok := off.AlertGroup(); ok || len(off.RuleGroups()) != 1 {
		t.Error("an SLO with both alert kinds off still has an alert group")
	}
	none, _ := Parse("none", "objective: 99\nindicator: {span: {service: a}}\nalerting: {min_events: 0}")
	if g, _ := none.AlertGroup(); strings.Contains(g.Rules[0].Expr, "offset 2m)) >=") {
		t.Errorf("min_events 0 still sets a minimum: %s", g.Rules[0].Expr)
	}

	// A calendar month has no budget alert; an objective so low that a
	// page would need more than every event failing says so.
	m, _ := Parse("m", "objective: 90\nwindow: calendar_month\nindicator: {probe: {name: x}}")
	g, _ = m.AlertGroup()
	for _, r := range g.Rules {
		if r.Alert == "SLOBudgetExhausted" {
			t.Error("a calendar-month SLO got SLOBudgetExhausted")
		}
	}
	w := strings.Join(m.Warnings(), "\n")
	if !strings.Contains(w, "SLOBurnFast over 1h can never fire") || !strings.Contains(w, "calendar month") {
		t.Errorf("warnings: %s", w)
	}
}

func TestWindowAndOperations(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	month, err := Parse("m", "objective: 99\nwindow: calendar_month\nindicator: {span: {service: shop, latency_below: 500ms}}\n")
	if err != nil {
		t.Fatal(err)
	}
	if from, to := month.WindowStart(now), month.WindowEnd(now); !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("calendar month: %s to %s", from, to)
	}
	bad, total := month.OperationsQueries(24 * time.Hour)
	if total != `sum by (span_name) (increase(traces_spans_total{service="shop", kind="server"}[1d]))` ||
		!strings.Contains(bad, `sum by (span_name) (increase(traces_span_duration_seconds_bucket{service="shop", kind="server", status="ok", le="0.5"}[1d]))`) {
		t.Errorf("operations: %s / %s", bad, total)
	}
	rolling, _ := Parse("r", "objective: 99\nindicator: {probe: {name: home}}\n")
	if from, to := rolling.WindowStart(now), rolling.WindowEnd(now); !from.Equal(now.Add(-30*24*time.Hour)) || !to.Equal(now) {
		t.Errorf("rolling: %s to %s", from, to)
	}
	if bad, _ := rolling.OperationsQueries(time.Hour); bad != "" {
		t.Errorf("a probe SLO has operations: %s", bad)
	}
}
