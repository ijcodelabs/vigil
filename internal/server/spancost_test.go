package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// The cost of a trace or a work item is the stored model calls' tokens at
// the configured prices; a model without a price counts tokens, not money.
func TestSpanCost(t *testing.T) {
	var asked string
	traces := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.FormValue("query")
		io.WriteString(w, `{"span_attr:gen_ai.response.model":"claude-sonnet-5-5-20261001","calls":"3","input_0":"1000000","output_0":"100000","cache_read_0":"","cache_write_0":""}`+"\n")
		io.WriteString(w, `{"span_attr:gen_ai.request.model":"local-llama","calls":"2","input_1":"5000","output_1":"700"}`+"\n")
	}))
	defer traces.Close()
	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL, cfg.Storage.TracesURL = traces.URL, traces.URL, traces.URL
	cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-sonnet-5", Input: 3, Output: 15}}
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	central := httptest.NewServer(srv.Handler())
	defer func() { central.Close(); srv.Close() }()
	c := newClient(t, central.URL)
	c.csrf = false
	get := func(q string) (int, string) {
		code, body, _ := c.do("GET", "/api/v1/costs/spans?"+q, "", "Authorization", "Bearer admin-token-secret")
		return code, body
	}

	for q, want := range map[string]int{
		"":                400,
		"trace=xyz":       400,
		"work_item=a%20b": 400,
		"trace=0af7651916cd43dd8448eb211c80319c&work_item=X": 400,
		"work_item=PORTUN-42&from=1&to=99999999":             400,
	} {
		if code, body := get(q); code != want {
			t.Errorf("%q: %d %s, want %d", q, code, body, want)
		}
	}

	code, body := get("trace=0AF7651916CD43DD8448EB211C80319C")
	var res struct {
		CostUSD float64 `json:"cost_usd"`
		Calls   int
		Models  []struct {
			Model   string
			Calls   int
			Tokens  map[string]float64
			CostUSD float64 `json:"cost_usd"`
			Priced  bool
		}
		Limits []string
	}
	if json.Unmarshal([]byte(body), &res); code != 200 || res.Calls != 5 || len(res.Models) != 2 {
		t.Fatalf("trace: %d %s", code, body)
	}
	// 1M input at 3/M plus 0.1M output at 15/M.
	if math.Abs(res.CostUSD-4.5) > 1e-9 || res.Models[0].Model != "claude-sonnet-5-5-20261001" || !res.Models[0].Priced {
		t.Errorf("priced: %+v", res)
	}
	if m := res.Models[1]; m.Priced || m.CostUSD != 0 || m.Tokens["input"] != 5000 || m.Tokens["output"] != 700 {
		t.Errorf("unpriced model, older token names: %+v", m)
	}
	if !strings.Contains(strings.Join(res.Limits, " "), "no price") {
		t.Errorf("limits: %v", res.Limits)
	}
	if !strings.HasPrefix(asked, `trace_id:="0af7651916cd43dd8448eb211c80319c"`) || !strings.Contains(asked, "| stats by") {
		t.Errorf("query for a trace: %s", asked)
	}

	if code, _ := get("work_item=PORTUN-42"); code != 200 ||
		!strings.Contains(asked, `"span_attr:work_item.key":="PORTUN-42" OR "resource_attr:work_item.key":="PORTUN-42"`) {
		t.Errorf("query for a work item: %d %s", code, asked)
	}
}
