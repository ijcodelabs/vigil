package server

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/ijcodelabs/vigil/internal/slo"
)

// Where each SLO stands now, worked out from its recorded minutes, for
// the SLOs page and the Home page. The figures are read slo.ReadOffset
// before now, as the alerts read them, so the page and the alerts agree.

// SLOStatus is one SLO over its window.
type SLOStatus struct {
	Name   string    `json:"name"`
	From   time.Time `json:"from"` // where the window starts
	To     time.Time `json:"to"`   // where it ends; later than now for a calendar month
	Good   float64   `json:"good"`
	Total  float64   `json:"total"`
	Budget float64   `json:"budget"` // the share of events allowed to be bad
	// Achieved is the share of good events in percent, BudgetLeft the share
	// of the budget not spent (1 is all of it, below 0 overspent). Both are
	// absent while the window holds no events.
	Achieved   *float64 `json:"achieved,omitempty"`
	BudgetLeft *float64 `json:"budget_left,omitempty"`
	// Burn is how fast the budget went over each of slo.BurnWindows, by
	// its name ("1h", "6h", "1d", "3d"); absent without events.
	Burn map[string]float64 `json:"burn"`
	// Counting is the first recorded minute in the window, and Coverage
	// the share of minutes since then that were recorded: a minute the
	// server did not evaluate counts as neither good nor bad.
	Counting *time.Time `json:"counting,omitempty"`
	Coverage *float64   `json:"coverage,omitempty"`
	Firing   []string   `json:"firing"` // the SLO's alerts that fire now
	Error    string     `json:"error,omitempty"`
}

func windowName(d time.Duration) string { return model.Duration(d).String() }

// sloStatus works out the status of every SLO in rows that parses. The
// queries go over all SLOs at once (by slo), a few per window length, so
// their number does not grow with the SLOs.
func (s *Server) sloStatus(ctx context.Context, rows []SLORow, now time.Time) ([]SLOStatus, error) {
	at := now.Add(-slo.ReadOffset)
	off := " offset " + model.Duration(slo.ReadOffset).String()
	query := func(expr string) (map[string]float64, error) {
		series, err := ruleBackend{s}.Query(ctx, "", expr, now)
		if err != nil {
			return nil, err
		}
		out := map[string]float64{}
		for _, sr := range series {
			out[sr.Labels["slo"]] = sr.Value
		}
		return out, nil
	}
	type sums struct{ good, total, minutes, first map[string]float64 }
	read := func(window string) (sums, error) {
		var r sums
		var err error
		for _, q := range []struct {
			dst  *map[string]float64
			expr string
		}{
			{&r.good, fmt.Sprintf(`sum by (slo) (sum_over_time(%s[%s]%s))`, slo.GoodMetric, window, off)},
			{&r.total, fmt.Sprintf(`sum by (slo) (sum_over_time(%s[%s]%s))`, slo.TotalMetric, window, off)},
			{&r.minutes, fmt.Sprintf(`sum by (slo) (count_over_time(%s[%s]%s))`, slo.TotalMetric, window, off)},
			{&r.first, fmt.Sprintf(`min by (slo) (tfirst_over_time(%s[%s]%s))`, slo.TotalMetric, window, off)},
		} {
			if *q.dst, err = query(q.expr); err != nil {
				return r, err
			}
		}
		return r, nil
	}

	firing := map[string][]string{}
	for _, g := range s.rules.Status() {
		if !strings.HasPrefix(g.Name, slo.GroupPrefix) {
			continue
		}
		for _, r := range g.Rules {
			for _, a := range r.Alerts {
				if a.State == "firing" && a.Labels["slo"] != "" {
					firing[a.Labels["slo"]] = append(firing[a.Labels["slo"]], r.Name)
				}
			}
		}
	}

	// The burn windows are the same for every SLO; the SLO windows are
	// read once per length (a calendar month's length is the time since
	// the first of the month).
	burns := map[string]sums{}
	for _, d := range slo.BurnWindows {
		r, err := read(windowName(d))
		if err != nil {
			return nil, err
		}
		burns[windowName(d)] = r
	}
	windows := map[string]sums{}
	out := []SLOStatus{}
	for _, row := range rows {
		if row.Error != "" {
			out = append(out, SLOStatus{Name: row.Name, Burn: map[string]float64{}, Firing: []string{}, Error: row.Error})
			continue
		}
		def := row.SLO
		from := def.WindowStart(now)
		w := windowName(max(at.Sub(from).Truncate(time.Minute), time.Minute))
		if def.Window != slo.CalendarMonth {
			w = windowName(def.WindowDuration())
		}
		sum, ok := windows[w]
		if !ok {
			var err error
			if sum, err = read(w); err != nil {
				return nil, err
			}
			windows[w] = sum
		}
		st := SLOStatus{Name: def.Name, From: from, To: def.WindowEnd(now), Good: sum.good[def.Name], Total: sum.total[def.Name],
			Budget: def.Budget(), Burn: map[string]float64{}, Firing: firing[def.Name]}
		if st.Firing == nil {
			st.Firing = []string{}
		}
		if st.Total > 0 {
			ratio := st.Good / st.Total
			achieved, left := 100*ratio, 1-(1-ratio)/st.Budget
			st.Achieved, st.BudgetLeft = &achieved, &left
		}
		for _, d := range slo.BurnWindows {
			b := burns[windowName(d)]
			if t := b.total[def.Name]; t > 0 {
				st.Burn[windowName(d)] = (1 - b.good[def.Name]/t) / st.Budget
			}
		}
		if first, ok := sum.first[def.Name]; ok && first > 0 {
			since := time.Unix(int64(first), 0).UTC()
			st.Counting = &since
			// The first minute is stamped at its end, so the span from it to
			// the last read minute holds one minute more than it measures.
			if expected := math.Floor(at.Sub(since).Minutes()) + 1; expected > 0 {
				c := math.Min(1, sum.minutes[def.Name]/expected)
				st.Coverage = &c
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// sloForecasts: SLOs whose budget is nearly spent while much of the
// window is still to come. An exhausted budget is an alert of its own
// (SLOBudgetExhausted), not a forecast.
func (s *Server) sloForecasts(ctx context.Context, now time.Time) ([]Coming, error) {
	rows, err := s.store.SLOs(defaultTenant)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	list, err := s.sloStatus(ctx, rows, now)
	if err != nil {
		return nil, err
	}
	var out []Coming
	for _, st := range list {
		if st.BudgetLeft == nil || *st.BudgetLeft <= 0 || *st.BudgetLeft >= 0.25 {
			continue
		}
		left := st.To.Sub(now)
		// A rolling window always has its whole length to come.
		if left < 7*24*time.Hour && st.To.After(now) {
			continue
		}
		c := Coming{Kind: "slo_budget", Resource: "slo:" + st.Name, Severity: "info",
			Title: fmt.Sprintf("SLO %s has %.0f%% of its error budget left", st.Name, 100**st.BudgetLeft),
			Link:  "#/slos/" + st.Name}
		if *st.BudgetLeft < 0.1 {
			c.Severity = "warning"
		}
		if st.To.After(now) {
			c.Title += fmt.Sprintf(", with %d days of the month to go", int(left.Hours()/24))
		}
		out = append(out, c)
	}
	return out, nil
}
