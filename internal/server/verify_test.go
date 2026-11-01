package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// verifyStore answers the verification's queries from a table: requests,
// failures and 95th percentile before and after the change.
type verifyStore struct {
	mu     sync.Mutex
	at     float64
	before [3]float64 // requests, failed, p95 seconds
	after  [3]float64
	seen   []string
}

func (v *verifyStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q, t := r.FormValue("query"), r.FormValue("time")
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, q)
	ts, _ := strconv.ParseFloat(t, 64)
	side := v.before
	if ts > v.at+1 {
		side = v.after
	}
	val := side[0]
	switch {
	case strings.Contains(q, "histogram_quantile"):
		val = side[2]
	case strings.Contains(q, `status="error"`):
		val = side[1]
	}
	result := []any{}
	if val >= 0 {
		result = append(result, map[string]any{"metric": map[string]string{}, "value": []any{ts, fmt.Sprint(val)}})
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}

func TestVerify(t *testing.T) {
	store := &verifyStore{}
	ts := httptest.NewServer(store)
	defer ts.Close()
	_, central := startServer(t, ts.URL)
	c := newClient(t, central.URL)
	c.csrf = false
	at := time.Now().Add(-time.Hour)
	store.at = float64(at.Unix())
	ask := func(query string) (int, map[string]any) {
		code, body, _ := c.do("GET", "/api/v1/verify?"+query, "", "Authorization", "Bearer admin-token-secret")
		var out map[string]any
		json.Unmarshal([]byte(body), &out)
		if out == nil {
			out = map[string]any{"body": body}
		}
		return code, out
	}
	atQ := "&at=" + strconv.FormatInt(at.Unix(), 10)

	for q, want := range map[string]int{
		"service=shop" + "&at=0":       400,
		"at=1":                         400,
		"service=shop&slo=x" + atQ:     400,
		"service=shop&window=1m" + atQ: 400,
		"service=shop&window=3h" + atQ: 400,
	} {
		if code, out := ask(q); code != want {
			t.Errorf("%s: %d %v, want %d", q, code, out, want)
		}
	}

	cases := []struct {
		name          string
		before, after [3]float64
		verdict       string
	}{
		{"failures up", [3]float64{900, 9, 0.2}, [3]float64{900, 90, 0.2}, "worse"},
		{"faster", [3]float64{900, 0, 0.6}, [3]float64{900, 0, 0.2}, "better"},
		{"faster but failing", [3]float64{900, 0, 0.6}, [3]float64{900, 90, 0.2}, "mixed"},
		{"the same", [3]float64{900, 9, 0.2}, [3]float64{950, 10, 0.21}, "no change"},
		{"quiet", [3]float64{10, 0, 0.2}, [3]float64{12, 0, 0.2}, "not enough data"},
		{"stopped", [3]float64{900, 0, 0.2}, [3]float64{0, 0, -1}, "worse"},
	}
	for _, tc := range cases {
		store.mu.Lock()
		store.before, store.after = tc.before, tc.after
		store.mu.Unlock()
		code, out := ask("service=shop" + atQ)
		if code != 200 || out["verdict"] != tc.verdict || out["window"] != "15m" {
			t.Errorf("%s: %d %v, want %s", tc.name, code, out, tc.verdict)
		}
	}
	store.mu.Lock()
	for _, q := range store.seen {
		if !strings.Contains(q, `service="shop"`) || !strings.Contains(q, "[15m]") {
			t.Errorf("query: %s", q)
		}
	}
	store.mu.Unlock()

	// A change ten minutes ago: both sides are what has passed since.
	recent := time.Now().Add(-10 * time.Minute)
	store.mu.Lock()
	store.at, store.before, store.after = float64(recent.Unix()), [3]float64{900, 9, 0.2}, [3]float64{900, 9, 0.2}
	store.mu.Unlock()
	if code, out := ask("service=shop&at=" + strconv.FormatInt(recent.Unix(), 10)); code != 200 || out["window"] != "9m" || out["verdict"] != "no change" {
		t.Errorf("recent change: %d %v", code, out)
	}
	// Two minutes ago: too soon.
	soon := time.Now().Add(-2 * time.Minute)
	if code, out := ask("service=shop&at=" + strconv.FormatInt(soon.Unix(), 10)); code != 200 || out["verdict"] != "not enough data" {
		t.Errorf("too soon: %d %v", code, out)
	}
	// A quote in the name stays inside the string.
	if code, _ := ask(`service=a"}or{x="` + atQ); code != 200 {
		t.Errorf("odd name: %d", code)
	}
	store.mu.Lock()
	last := store.seen[len(store.seen)-1]
	store.mu.Unlock()
	if !strings.Contains(last, `service="a\"}or{x=\""`) {
		t.Errorf("the name was not quoted: %s", last)
	}
}
