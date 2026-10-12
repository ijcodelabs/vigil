package dockerapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A container that ends between the list and the question about it is
// skipped quietly; a real failure is still reported, and the rest is kept.
func TestMetricsSkipsVanishedContainers(t *testing.T) {
	broken := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"aaa","Names":["/web"],"Image":"nginx","State":"running"},
				{"Id":"bbb","Names":["/healthcheck"],"Image":"busybox","State":"running"},
				{"Id":"ccc","Names":["/db"],"Image":"postgres","State":"running"}]`)
		case strings.Contains(r.URL.Path, "/bbb/"):
			http.Error(w, `{"message":"No such container: bbb"}`, http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/ccc/") && broken:
			http.Error(w, "daemon on fire", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"State":{"Running":true}}`)
		default:
			io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":2000000000}},"memory_stats":{"usage":1048576,"limit":2097152}}`)
		}
	}))
	defer api.Close()
	c := NewHTTP(api.URL)

	samples, err := c.Metrics(context.Background())
	seen := map[string]bool{}
	for _, s := range samples {
		if s.Name == "container_memory_usage_bytes" {
			seen[s.Labels["container"]] = true
		}
	}
	if err != nil || !seen["web"] || !seen["db"] || seen["healthcheck"] {
		t.Errorf("with a vanished container: err=%v containers=%v", err, seen)
	}

	broken = true
	samples, err = c.Metrics(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || len(samples) == 0 {
		t.Errorf("with a failing daemon: err=%v, %d samples", err, len(samples))
	}
}

// A stopped container is still reported, with its exit code and without
// usage figures; one that was only created is not reported at all.
func TestMetricsReportStoppedContainers(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"aaa","Names":["/web"],"Image":"nginx","State":"running"},
				{"Id":"bbb","Names":["/worker"],"Image":"app","State":"exited"},
				{"Id":"ccc","Names":["/migrate"],"Image":"app","State":"exited"},
				{"Id":"ddd","Names":["/new"],"Image":"app","State":"created"}]`)
		case strings.HasSuffix(r.URL.Path, "/aaa/json"):
			io.WriteString(w, `{"State":{"Running":true,"ExitCode":0},"RestartCount":2}`)
		case strings.HasSuffix(r.URL.Path, "/bbb/json"):
			io.WriteString(w, `{"State":{"Running":false,"ExitCode":137},"RestartCount":0}`)
		case strings.HasSuffix(r.URL.Path, "/ccc/json"):
			io.WriteString(w, `{"State":{"Running":false,"ExitCode":0},"RestartCount":0}`)
		case strings.HasSuffix(r.URL.Path, "/aaa/stats"):
			io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":2000000000}},"memory_stats":{"usage":1048576,"limit":2097152}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	samples, err := NewHTTP(api.URL).Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		got[s.Labels["container"]+" "+s.Name] = s.Value
	}
	for k, want := range map[string]float64{
		"web container_running": 1, "web container_restarts_total": 2, "web container_memory_usage_bytes": 1048576,
		"worker container_running": 0, "worker container_exit_code": 137,
		"migrate container_running": 0, "migrate container_exit_code": 0,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}
	for _, k := range []string{"web container_exit_code", "worker container_memory_usage_bytes", "new container_running"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should not be reported", k)
		}
	}
}
