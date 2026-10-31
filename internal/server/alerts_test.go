package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/server"
)

// alertStores stands in for both stores' query and write endpoints.
type alertStores struct {
	mu      sync.Mutex
	cpuHigh bool
	written []model.Sample
}

func (a *alertStores) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := r.URL.Query().Get("query")
	vector := func(rows string) {
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, rows)
	}
	switch r.URL.Path {
	case "/api/v1/query":
		switch {
		case q == "bad(":
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"status":"error","errorType":"422","error":"cannot parse \"bad(\": missing closing parenthesis"}`)
		case q == "cpu_high" && a.cpuHigh:
			vector(`{"metric":{"__name__":"cpu","instance":"web-01","agent":"web-01"},"value":[1700000000,"97.5"]}`)
		case strings.HasPrefix(q, "group by (probe) (last_over_time(probe_success[1d]))"):
			// Checks the SLO templates offer an SLO for; one name cannot go
			// into a document.
			vector(`{"metric":{"probe":"shop-home"},"value":[1700000000,"1"]},{"metric":{"probe":"has \"quotes\""},"value":[1700000000,"1"]}`)
		case strings.Contains(q, "_per_minute["):
			// The SLO page's sums over all SLOs (the alerts select one SLO
			// and get nothing): SLO home counted a day, 9995 good of 10000
			// in its window and 990 of 1000 in the last hour.
			v := map[string]string{
				"sum_over_time(slo_good_per_minute[30d]": "9995", "sum_over_time(slo_total_per_minute[30d]": "10000",
				"count_over_time(slo_total_per_minute[30d]":  "1440",
				"tfirst_over_time(slo_total_per_minute[30d]": strconv.FormatInt(time.Now().Add(-24*time.Hour).Unix(), 10),
				"sum_over_time(slo_good_per_minute[1h]":      "990", "sum_over_time(slo_total_per_minute[1h]": "1000",
				// August 2026 for the SLA report, counted all month.
				"sum_over_time(slo_good_per_minute[31d]": "9995", "sum_over_time(slo_total_per_minute[31d]": "10000",
				"count_over_time(slo_total_per_minute[31d]":  "44640",
				"tfirst_over_time(slo_total_per_minute[31d]": "1785542460",
			}
			for k, x := range v {
				if strings.Contains(q, k) {
					vector(`{"metric":{"slo":"home"},"value":[1700000000,"` + x + `"]}`)
					return
				}
			}
			vector("")
		default:
			vector("")
		}
	case "/api/v1/query_range":
		// Per day for the SLA report: the first two days of the range, the
		// first with 5 bad events in 5000, the second with none.
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		v := [2]string{"4995", "5000"}
		if strings.Contains(q, "slo_total_per_minute") {
			v = [2]string{"5000", "5000"}
		}
		if !strings.Contains(q, "_per_minute[1d]") {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"slo":"home"},"values":[[%d,%q],[%d,%q]]}]}}`,
			start, v[0], start+86400, v[1])
	case "/select/logsql/stats_query":
		if strings.Contains(q, "error") {
			vector(`{"metric":{"__name__":"errors","app":"shop"},"value":[1700000000,"12"]}`)
		} else {
			vector("")
		}
	case "/api/v1/write":
		body, _ := io.ReadAll(r.Body)
		ss, _ := promwire.Decode(body)
		// Like the real store, extra_label overrides a label of the same name.
		if k, v, ok := strings.Cut(r.URL.Query().Get("extra_label"), "="); ok {
			for _, s := range ss {
				s.Labels[k] = v
			}
		}
		a.written = append(a.written, ss...)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

type hookAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func TestRulesToNotification(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stores := &alertStores{cpuHigh: true}
	backend := httptest.NewServer(stores)
	defer backend.Close()

	var mu sync.Mutex
	var received []hookAlert
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var msg struct {
			Alerts []hookAlert `json:"alerts"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		received = append(received, msg.Alerts...)
		mu.Unlock()
	}))
	defer hook.Close()
	got := func(name, status string) *hookAlert {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range received {
			if a.Labels["alertname"] == name && a.Status == status {
				return &a
			}
		}
		return nil
	}

	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin",
		ExternalURL: "http://vigil.example:8080", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = backend.URL, backend.URL
	srv, err := server.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	do := func(method, path, token, body string) (int, string) {
		req, _ := http.NewRequest(method, central.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if strings.HasPrefix(body, "{") {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Defaults before anything is configured.
	if code, body := do("GET", "/api/v1/alerting/config", "admin", ""); code != 200 || !strings.Contains(body, "receiver: none") {
		t.Fatalf("default alerting config: %d %q", code, body)
	}
	if code, body := do("GET", "/api/v1/rules", "admin", ""); code != 200 || !strings.Contains(body, `"groups":[]`) {
		t.Fatalf("empty rules: %d %q", code, body)
	}

	// Invalid configs are refused with a reason and change nothing.
	if code, body := do("PUT", "/api/v1/alerting/config", "admin", "route: {receiver: missing}\n"); code != 400 || !strings.Contains(body, "missing") {
		t.Errorf("bad alerting config: %d %q", code, body)
	}
	if code, body := do("PUT", "/api/v1/rules/config", "admin", "groups: [{name: g, rules: [{alert: A, expr: 'bad('}]}]"); code != 400 ||
		!strings.Contains(body, "missing closing parenthesis") || !strings.Contains(body, `rule "A"`) {
		t.Errorf("bad expr: %d %q", code, body)
	}
	if code, body := do("PUT", "/api/v1/rules/config", "admin", "groups: [{name: g, rules: [{alert: A, expr: up, fro: 1m}]}]"); code != 400 || !strings.Contains(body, "fro") {
		t.Errorf("typo in rule: %d %q", code, body)
	}
	if code, _ := do("PUT", "/api/v1/rules/config", "ingest", "groups: []"); code != http.StatusUnauthorized {
		t.Errorf("ingest token may change rules: %d", code)
	}

	alerting := `
route:
  receiver: hook
  group_by: [alertname]
  group_wait: 10ms
  group_interval: 100ms
  repeat_interval: 1h
receivers:
  - name: hook
    webhook_configs:
      - url: ` + hook.URL + `
        send_resolved: true
`
	if code, body := do("PUT", "/api/v1/alerting/config", "admin", alerting); code != http.StatusNoContent {
		t.Fatalf("alerting config: %d %q", code, body)
	}
	rules := `
groups:
  - name: host
    interval: 50ms
    labels: {team: ops}
    rules:
      - alert: CPUHigh
        expr: cpu_high
        for: 100ms
        labels: {severity: page}
        annotations:
          summary: 'CPU on {{ $labels.instance }} is {{ $value | humanize }}%'
      - record: instance:cpu:high
        expr: cpu_high
  - name: logs
    type: logs
    interval: 50ms
    rules:
      - alert: ErrorBurst
        expr: '_time:5m error | stats by (app) count() as errors | filter errors:>10'
        annotations:
          summary: '{{ $value }} errors in {{ $labels.app }}'
`
	if code, body := do("PUT", "/api/v1/rules/config", "admin", rules); code != http.StatusNoContent {
		t.Fatalf("rules: %d %q", code, body)
	}
	if code, body := do("GET", "/api/v1/rules/config", "admin", ""); code != 200 || body != rules {
		t.Errorf("rules not stored verbatim: %d", code)
	}

	waitFor(t, "CPUHigh notification", func() bool { return got("CPUHigh", "firing") != nil })
	cpu := got("CPUHigh", "firing")
	if cpu.Labels["instance"] != "web-01" || cpu.Labels["severity"] != "page" || cpu.Labels["team"] != "ops" ||
		cpu.Annotations["summary"] != "CPU on web-01 is 97.5%" {
		t.Errorf("CPUHigh: %+v", cpu)
	}
	waitFor(t, "ErrorBurst notification", func() bool { return got("ErrorBurst", "firing") != nil })
	if a := got("ErrorBurst", "firing"); a.Labels["app"] != "shop" || a.Annotations["summary"] != "12 errors in shop" {
		t.Errorf("ErrorBurst: %+v", a)
	}

	// Status APIs.
	code, body := do("GET", "/api/v1/alerts", "admin", "")
	if code != 200 || !strings.Contains(body, `"alertname":"CPUHigh"`) || !strings.Contains(body, `"state":"firing"`) {
		t.Errorf("alerts API: %d %s", code, body)
	}
	if code, body := do("GET", "/api/v1/rules", "admin", ""); code != 200 || !strings.Contains(body, `"health":"ok"`) ||
		!strings.Contains(body, `"name":"instance:cpu:high"`) || strings.Contains(body, `"health":"err"`) {
		t.Errorf("rules API: %d %s", code, body)
	}
	// The embedded Alertmanager API: list alerts, create a silence.
	if code, body := do("GET", "/alertmanager/api/v2/alerts", "admin", ""); code != 200 || !strings.Contains(body, "ErrorBurst") {
		t.Errorf("alertmanager alerts: %d %s", code, body)
	}
	silence := fmt.Sprintf(`{"matchers":[{"name":"alertname","value":"ErrorBurst","isRegex":false}],"startsAt":%q,"endsAt":%q,"createdBy":"test","comment":"known"}`,
		time.Now().UTC().Format(time.RFC3339), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if code, body := do("POST", "/alertmanager/api/v2/silences", "admin", silence); code != 200 || !strings.Contains(body, "silenceID") {
		t.Errorf("create silence: %d %s", code, body)
	}
	// A script with a token may say whom it acts for; without a name it is named itself.
	if code, body := do("POST", "/alertmanager/api/v2/silences", "admin", strings.Replace(silence, `"createdBy":"test"`, `"createdBy":""`, 1)); code != 200 {
		t.Errorf("create unsigned silence: %d %s", code, body)
	}
	if code, body := do("GET", "/alertmanager/api/v2/silences", "admin", ""); code != 200 || !strings.Contains(body, `"createdBy":"test"`) || !strings.Contains(body, `"createdBy":"admin-token"`) {
		t.Errorf("who made the silences: %d %s", code, body)
	}
	if code, _ := do("GET", "/alertmanager/api/v2/silences", "", ""); code != http.StatusUnauthorized {
		t.Errorf("alertmanager API without token: %d", code)
	}

	// Rule output is written back to the metric store.
	waitFor(t, "ALERTS and recorded series", func() bool {
		stores.mu.Lock()
		defer stores.mu.Unlock()
		var alerts, rec bool
		for _, s := range stores.written {
			// The agent label must still name the host the result is about.
			alerts = alerts || (s.Name == "ALERTS" && s.Labels["alertname"] == "CPUHigh" && s.Labels["alertstate"] == "firing" && s.Labels["agent"] == "web-01")
			rec = rec || (s.Name == "instance:cpu:high" && s.Value == 97.5 && s.Labels["team"] == "ops" && s.Labels["agent"] == "web-01")
		}
		return alerts && rec
	})

	// The condition clears: a resolved notification follows.
	stores.mu.Lock()
	stores.cpuHigh = false
	stores.mu.Unlock()
	waitFor(t, "CPUHigh resolution", func() bool { return got("CPUHigh", "resolved") != nil })
}
