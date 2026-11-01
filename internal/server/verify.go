package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/slo"
)

// Verification answers "did this change help?" for a service, or an SLO:
// the same length of time before and after a moment, compared on what the
// agents count for every request (rate, failures, 95th percentile) or on
// the SLO's good and total minutes. The verdict is advice with its
// reasons and limits; a person, or Boop's policy, decides what to do with
// it (agentic.md). Fixed queries over bounded windows only: nothing the
// caller writes reaches the store as a query.

const (
	verifyMinRequests = 20               // fewer on either side: not enough data
	verifyMinWindow   = 5 * time.Minute  // the shortest comparison worth making
	verifyMaxWindow   = 2 * time.Hour    // and the longest
	verifyLag         = 30 * time.Second // VictoriaMetrics hides the newest 30 s
	verifyDefault     = 15 * time.Minute
)

// Verdicts.
const (
	verdictBetter = "better"
	verdictWorse  = "worse"
	verdictMixed  = "mixed"
	verdictSame   = "no change"
	verdictNoData = "not enough data"
)

type verifySignal struct {
	Name    string   `json:"name"`
	Before  *float64 `json:"before"`
	After   *float64 `json:"after"`
	Unit    string   `json:"unit"`
	Verdict string   `json:"verdict"`
	Why     string   `json:"why,omitempty"`
}

type verifyResult struct {
	Service string         `json:"service,omitempty"`
	SLO     string         `json:"slo,omitempty"`
	At      time.Time      `json:"at"`
	Window  string         `json:"window"`
	Verdict string         `json:"verdict"`
	Signals []verifySignal `json:"signals"`
	Limits  []string       `json:"limits"`
}

func (s *Server) verifyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/verify", s.viewer(s.handleVerify))
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service, sloName := strings.TrimSpace(q.Get("service")), strings.TrimSpace(q.Get("slo"))
	if (service == "") == (sloName == "") || len(service) > 200 || len(sloName) > 200 {
		http.Error(w, "give either service or slo", http.StatusBadRequest)
		return
	}
	at, err := strconv.ParseFloat(q.Get("at"), 64)
	if err != nil || at <= 0 {
		http.Error(w, "at is the moment of the change, in Unix seconds", http.StatusBadRequest)
		return
	}
	window := verifyDefault
	if v := q.Get("window"); v != "" {
		if window, err = time.ParseDuration(v); err != nil || window < verifyMinWindow || window > verifyMaxWindow {
			http.Error(w, "window is 5m to 2h", http.StatusBadRequest)
			return
		}
	}
	res, err := s.verify(r.Context(), service, sloName, time.Unix(0, int64(at*1e9)), window, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, res)
}

// verify compares [at-window, at) with [at, at+window). When less than
// the window has passed since the change, both sides shrink to what has.
func (s *Server) verify(ctx context.Context, service, sloName string, at time.Time, window time.Duration, now time.Time) (verifyResult, error) {
	res := verifyResult{Service: service, SLO: sloName, At: at.UTC(), Signals: []verifySignal{}, Limits: []string{}}
	if sloName != "" {
		now = now.Add(-slo.ReadOffset) // its minutes are recorded a little late
	}
	if avail := now.Add(-verifyLag).Sub(at); avail < window {
		window = avail.Truncate(time.Minute)
		res.Limits = append(res.Limits, "Less time has passed since the change than the window asked for; both sides are "+windowName(window))
	}
	res.Window = windowName(window)
	if window < verifyMinWindow {
		res.Window = windowName(max(window, 0))
		res.Verdict = verdictNoData
		res.Limits = append(res.Limits, "Wait until at least 5 minutes have passed since the change")
		return res, nil
	}
	w := windowName(window)
	query := func(expr string, t time.Time) (*float64, error) {
		series, err := ruleBackend{s}.Query(ctx, "", expr, t)
		if err != nil {
			return nil, err
		}
		if len(series) == 0 || math.IsNaN(series[0].Value) || math.IsInf(series[0].Value, 0) {
			return nil, nil
		}
		v := series[0].Value
		return &v, nil
	}
	pair := func(expr string) (before, after *float64, err error) {
		if before, err = query(expr, at); err == nil {
			after, err = query(expr, at.Add(window))
		}
		return before, after, err
	}
	if sloName != "" {
		return s.verifySLO(res, sloName, w, pair)
	}
	sel := fmt.Sprintf(`service=%s,kind=~"server|consumer"`, strconv.Quote(service))
	reqB, reqA, err := pair(fmt.Sprintf(`sum(increase(traces_spans_total{%s}[%s]))`, sel, w))
	if err != nil {
		return res, err
	}
	errB, errA, err := pair(fmt.Sprintf(`sum(increase(traces_spans_total{%s,status="error"}[%s]))`, sel, w))
	if err != nil {
		return res, err
	}
	p95B, p95A, err := pair(fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (increase(traces_span_duration_seconds_bucket{%s}[%s])))`, sel, w))
	if err != nil {
		return res, err
	}
	n := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	rate := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := *p / window.Seconds()
		return &v
	}
	reqSig := verifySignal{Name: "requests", Before: rate(reqB), After: rate(reqA), Unit: "ops/s", Verdict: verdictSame}
	switch {
	case n(reqB) >= verifyMinRequests && n(reqA) < 1:
		reqSig.Verdict, reqSig.Why = verdictWorse, "requests stopped after the change"
	case n(reqB) < verifyMinRequests || n(reqA) < verifyMinRequests:
		res.Signals = append(res.Signals, reqSig)
		res.Verdict = verdictNoData
		res.Limits = append(res.Limits, fmt.Sprintf("Fewer than %d requests on one side of the change", verifyMinRequests))
		return res, nil
	}
	res.Signals = append(res.Signals, reqSig)
	if reqSig.Verdict == verdictWorse {
		res.Verdict = verdictWorse
		return res, nil
	}
	share := func(e, r *float64) *float64 {
		v := 100 * n(e) / n(r)
		return &v
	}
	eb, ea := share(errB, reqB), share(errA, reqA)
	errSig := verifySignal{Name: "failed requests", Before: eb, After: ea, Unit: "percent", Verdict: compareUp(*eb, *ea, 1, 2)}
	if errSig.Verdict != verdictSame {
		errSig.Why = fmt.Sprintf("%.1f%% of requests failed before, %.1f%% after", *eb, *ea)
	}
	res.Signals = append(res.Signals, errSig)
	latSig := verifySignal{Name: "95th percentile", Before: p95B, After: p95A, Unit: "seconds", Verdict: verdictSame}
	if p95B != nil && p95A != nil {
		latSig.Verdict = compareUp(*p95B, *p95A, 0.05, 1.5)
		if latSig.Verdict != verdictSame {
			latSig.Why = fmt.Sprintf("%.0f ms before, %.0f ms after", 1000**p95B, 1000**p95A)
		}
	}
	res.Signals = append(res.Signals, latSig)
	res.Verdict = combine(res.Signals)
	res.Limits = append(res.Limits, "It counts every request the agents saw; other changes in the same time, and load, are not told apart")
	return res, nil
}

func (s *Server) verifySLO(res verifyResult, name, w string, pair func(string) (*float64, *float64, error)) (verifyResult, error) {
	sel := "slo=" + strconv.Quote(name)
	off := " offset " + windowName(slo.ReadOffset)
	goodB, goodA, err := pair(fmt.Sprintf(`sum(sum_over_time(%s{%s}[%s]%s))`, slo.GoodMetric, sel, w, off))
	if err != nil {
		return res, err
	}
	totB, totA, err := pair(fmt.Sprintf(`sum(sum_over_time(%s{%s}[%s]%s))`, slo.TotalMetric, sel, w, off))
	if err != nil {
		return res, err
	}
	if totB == nil || totA == nil || *totB < verifyMinRequests || *totA < verifyMinRequests {
		res.Verdict = verdictNoData
		res.Limits = append(res.Limits, fmt.Sprintf("Fewer than %d events on one side of the change", verifyMinRequests))
		return res, nil
	}
	bad := func(g, t *float64) *float64 {
		v := 100 * (1 - deref(g) / *t)
		return &v
	}
	bb, ba := bad(goodB, totB), bad(goodA, totA)
	sig := verifySignal{Name: "bad events", Before: bb, After: ba, Unit: "percent", Verdict: compareUp(*bb, *ba, 0.1, 2)}
	if sig.Verdict != verdictSame {
		sig.Why = fmt.Sprintf("%.2f%% of events were bad before, %.2f%% after", *bb, *ba)
	}
	res.Signals = append(res.Signals, sig)
	res.Verdict = sig.Verdict
	res.Limits = append(res.Limits, "It reads the SLO's own minutes; other changes in the same time are not told apart")
	return res, nil
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// compareUp says how a value where more is worse moved: worse when it grew
// by at least minDiff and to at least factor times what it was, better the
// other way round, otherwise no change.
func compareUp(before, after, minDiff, factor float64) string {
	switch {
	case after-before >= minDiff && after >= factor*before:
		return verdictWorse
	case before-after >= minDiff && before >= factor*after:
		return verdictBetter
	}
	return verdictSame
}

func combine(signals []verifySignal) string {
	worse, better := false, false
	for _, sg := range signals {
		worse = worse || sg.Verdict == verdictWorse
		better = better || sg.Verdict == verdictBetter
	}
	switch {
	case worse && better:
		return verdictMixed
	case worse:
		return verdictWorse
	case better:
		return verdictBetter
	}
	return verdictSame
}
