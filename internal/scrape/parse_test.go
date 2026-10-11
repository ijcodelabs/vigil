package scrape

import (
	"math"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `# HELP http_requests_total Requests.
# TYPE http_requests_total counter
http_requests_total{method="get",code="200"} 1027 1395066363000
http_requests_total{ method="post", code="400", } 3
msg{text="a \"quoted\" \\ back\nslash, with = and }"} 1
plain 2.5e3
lat_bucket{le="+Inf"} +Inf
nan_value NaN # {trace_id="x"} 1
`
	got, err := Parse(strings.NewReader(in), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d samples", len(got))
	}
	if got[0].Name != "http_requests_total" || got[0].Labels["code"] != "200" || got[0].Value != 1027 || got[0].TS != 42 {
		t.Errorf("sample 0: %+v", got[0])
	}
	if got[1].Labels["method"] != "post" || len(got[1].Labels) != 2 {
		t.Errorf("sample 1: %+v", got[1])
	}
	if want := "a \"quoted\" \\ back\nslash, with = and }"; got[2].Labels["text"] != want {
		t.Errorf("escapes: %q", got[2].Labels["text"])
	}
	if got[3].Value != 2500 || !math.IsInf(got[4].Value, 1) || !math.IsNaN(got[5].Value) {
		t.Errorf("values: %v %v %v", got[3].Value, got[4].Value, got[5].Value)
	}
}

func TestParseRejectsHTML(t *testing.T) {
	for _, in := range []string{"<html><body>404</body></html>", `m{a="b} 1`, "m{a=b} 1", "m abc"} {
		if _, err := Parse(strings.NewReader(in), 0); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}
