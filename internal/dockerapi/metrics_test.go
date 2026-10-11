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
		default:
			io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":2000000000}},"memory_stats":{"usage":1048576,"limit":2097152}}`)
		}
	}))
	defer api.Close()
	c := NewHTTP(api.URL)

	samples, err := c.Metrics(context.Background())
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.Labels["container"]] = true
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
