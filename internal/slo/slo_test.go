package slo

import (
	"strings"
	"testing"
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
