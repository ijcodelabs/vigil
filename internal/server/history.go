package server

import (
	_ "embed"
	"fmt"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/ijcodelabs/vigil/internal/rules"
)

// The history store (storage.history_url) is a second VictoriaMetrics with
// a long retention. The main store keeps every sample for a short while;
// the history keeps hourly rollups of what is worth having for months or
// years (history_rules.yaml, and any rule group with `store: history`).
// A store's retention is one setting for all it holds, which is why the
// rollups need a store of their own.

//go:embed history_rules.yaml
var historyRules string

func historyRuleGroups() []rules.Group {
	f, err := rules.Parse(historyRules)
	if err != nil {
		panic(fmt.Sprintf("history_rules.yaml: %v", err)) // a build mistake; the test catches it
	}
	return f.Groups
}

const (
	// historyDelay is how long after an hour its rollup is made: the
	// per-minute series it sums are recorded AlignDelay late themselves.
	historyDelay = 5 * time.Minute
	// historyBackfill is how far back a history group's first run reaches
	// into the main store, and historyBackfillEvals bounds it for a group
	// with a short interval.
	historyBackfill      = 90 * 24 * time.Hour
	historyBackfillEvals = 2200
)

// alignHistory makes a history group record each interval once, at its
// end (as alignPerMinute does), and fill in the past on its first run.
func alignHistory(g *rules.Group) {
	if g.Store != rules.StoreHistory {
		return
	}
	every := time.Duration(g.Interval)
	if every <= 0 {
		every = time.Minute
	}
	if g.Delay == 0 {
		g.Delay = historyDelay
	}
	g.Backfill = min(historyBackfill, historyBackfillEvals*every)
}

// historyRoutes serves the Prometheus query API of the history store under
// /api/v1/history/, for dashboards and Explore.
// Without a history store a panel that reads it says how to get one.
func (s *Server) historyRoutes(mux *http.ServeMux) {
	var proxy *httputil.ReverseProxy
	if s.history != nil {
		proxy = httputil.NewSingleHostReverseProxy(s.history)
	}
	query := s.asks(func(w http.ResponseWriter, r *http.Request) {
		if proxy == nil {
			http.Error(w, "no history store: this server keeps no long-term rollups (storage.history_url, see /docs/install.md)", http.StatusNotFound)
			return
		}
		op := r.PathValue("op")
		if op != "query" && op != "query_range" && op != "series" && op != "labels" {
			http.NotFound(w, r)
			return
		}
		r.URL.Path, r.URL.RawPath = "/api/v1/"+op, ""
		proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/v1/history/{op}", query)
	mux.HandleFunc("POST /api/v1/history/{op}", query)
}
