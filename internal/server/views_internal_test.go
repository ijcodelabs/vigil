package server

import (
	"strings"
	"testing"
)

// What a view's queries may not do, one case each.
func TestCheckView(t *testing.T) {
	hosts := map[string]string{"h1": "web-1"}
	for _, c := range []struct {
		typ, source, expr, want string
	}{
		{"timeseries", "metrics", `rate(host_cpu_seconds_total{agent="@h1"}[5m])`, ""},
		{"stat", "metrics", `increase(x[$__range])`, ""},
		{"logs", "logs", `agent:="@h1" error`, ""},
		{"table", "logs", `* | stats by (status) count() hits`, ""},
		{"timeseries", "metrics", `x{agent=~"$agent"}`, "variable"},
		{"timeseries", "metrics", `rate(x[7d])`, "up to 1d"},
		{"logs", "logs", `error | stats count()`, "shows lines"},
		{"stat", "logs", `error`, "needs | stats"},
		{"stat", "logs", `_time:1h error | stats count()`, "leave _time out"},
		{"timeseries", "metrics", `x{agent=~"@h1"}`, "host reference"},
		{"timeseries", "metrics", `x{agent="@h1-other"}`, "host reference"},
		{"timeseries", "metrics", `x{agent="@h2"}`, "no host vigil gave"},
		{"map", "metrics", `up`, "maps"},
	} {
		d := &Dashboard{Title: "t", Panels: []Panel{{Title: "p", Type: c.typ, Source: c.source, W: 6, H: 3, Queries: []Query{{Expr: c.expr}}}}}
		err := checkView(d, hosts)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.expr, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.expr, err, c.want)
		}
	}
}

func TestMdSections(t *testing.T) {
	md := "# A\n\n## B\nb text\n### B1\nmore\n## C\nc text\n"
	got := mdSections(md, "B")
	if len(got) != 1 || !strings.Contains(got[0], "more") || strings.Contains(got[0], "c text") {
		t.Errorf("%q", got)
	}
}
