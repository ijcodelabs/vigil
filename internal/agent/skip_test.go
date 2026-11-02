package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

// TestManagedConfigFromNewerServer: a setting this agent does not know is
// skipped, the rest applied, and the agent says what it skipped; a guarded
// one still refuses the whole config.
func TestManagedConfigFromNewerServer(t *testing.T) {
	var mu sync.Mutex
	var reports []map[string]any
	doc := "interval: 20s\nfuture_thing: {x: 1}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep map[string]any
		json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		reports = append(reports, rep)
		d := doc
		mu.Unlock()
		etag := `"` + config.Hash(d) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(d))
	}))
	defer srv.Close()
	a := testAgent(t, srv.URL)
	a.cfg.LocalLabels = map[string]string{}
	a.PollEvery = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan *config.Collection, 1)
	go a.pollConfig(ctx, filepath.Join(a.cfg.SpoolDir, "managed-config.yaml"), updates)

	select {
	case c := <-updates:
		if c.Interval.Std() != 20*time.Second {
			t.Errorf("interval %v", c.Interval)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the config was not applied")
	}
	last := func(key string) any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(reports)
			var v any
			if n > 1 {
				v = reports[n-1][key]
			}
			mu.Unlock()
			if v != nil && v != "" {
				return v
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	if got, _ := json.Marshal(last("config_skipped")); string(got) != `["future_thing"]` {
		t.Errorf("reported skipped: %s", got)
	}

	// A new setting under tls: refused, the agent keeps what it runs.
	mu.Lock()
	doc = "interval: 30s\notlp:\n  tls:\n    from_future: x\n"
	mu.Unlock()
	if e, _ := last("config_error").(string); e == "" {
		t.Error("a guarded unknown setting was not reported as an error")
	}
	select {
	case c := <-updates:
		t.Errorf("a guarded config was applied: %v", c.Interval)
	default:
	}
}
