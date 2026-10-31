package server

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/ijcodelabs/vigil/internal/slo"
)

// SLA reports: per calendar month or quarter, what every SLO achieved.
// A closed period's report is worked out once and stored, so a later
// change to an SLO, to the incidents kept or to the metric store's
// retention does not change a report that was already given. An open
// period (the current month) is worked out on each request and marked
// as not final. Spec: memory/context/slo.md, "SLA report".

// SLOReport is one period.
type SLOReport struct {
	Period  string         `json:"period"` // 2026-10 or 2026-Q4
	From    time.Time      `json:"from"`
	To      time.Time      `json:"to"`
	Final   bool           `json:"final"` // the period is over and the report stored
	Created time.Time      `json:"created"`
	SLOs    []SLOReportRow `json:"slos"`
}

// SLOReportRow is one SLO over the period.
type SLOReportRow struct {
	Name        string  `json:"name"`
	Service     string  `json:"service,omitempty"`
	Owner       string  `json:"owner,omitempty"`
	Description string  `json:"description,omitempty"`
	Kind        string  `json:"kind"`
	Objective   float64 `json:"objective"` // percent, the SLO's when the report was made
	// ObjectiveChanged is set when the recorded objective was not the same
	// all through the period: the achieved share is then judged against
	// the objective at the end.
	ObjectiveChanged bool    `json:"objective_changed,omitempty"`
	Good             float64 `json:"good"`
	Total            float64 `json:"total"`
	Bad              float64 `json:"bad"`
	// Achieved, Met and BudgetUsed (1 is the whole budget) are absent
	// without events in the period.
	Achieved   *float64 `json:"achieved,omitempty"`
	Met        *bool    `json:"met,omitempty"`
	BudgetUsed *float64 `json:"budget_used,omitempty"`
	// Coverage is the share of the period's minutes that were counted;
	// Counting the first one, when the SLO started counting in the period.
	Coverage  float64             `json:"coverage"`
	Counting  *time.Time          `json:"counting,omitempty"`
	WorstDay  *SLOReportDay       `json:"worst_day,omitempty"`
	Incidents []SLOReportIncident `json:"incidents"`
}

// SLOReportDay is the UTC day with the largest share of bad events.
type SLOReportDay struct {
	Day      string  `json:"day"` // 2026-10-14
	Achieved float64 `json:"achieved"`
	Bad      float64 `json:"bad"`
}

type SLOReportIncident struct {
	Alert      string    `json:"alert"`
	Severity   string    `json:"severity,omitempty"`
	Title      string    `json:"title"`
	Opened     time.Time `json:"opened"`
	Resolved   time.Time `json:"resolved,omitzero"`
	Seconds    float64   `json:"seconds"` // open for, within the period
	AckBy      string    `json:"ack_by,omitempty"`
	AckComment string    `json:"ack_comment,omitempty"`
}

var periodRE = regexp.MustCompile(`^(\d{4})-(?:(0[1-9]|1[0-2])|Q([1-4]))$`)

// reportSettle is how long after a period ends its report waits: the last
// minutes are counted 90 seconds after they end.
const reportSettle = 10 * time.Minute

// parsePeriod reads "2026-10" (a month) or "2026-Q4" (a quarter), in UTC.
func parsePeriod(p string) (from, to time.Time, err error) {
	m := periodRE.FindStringSubmatch(p)
	if m == nil {
		return from, to, fmt.Errorf("period %q: a month (2026-10) or a quarter (2026-Q4)", p)
	}
	year, _ := strconv.Atoi(m[1])
	if m[2] != "" {
		month, _ := strconv.Atoi(m[2])
		from = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		return from, from.AddDate(0, 1, 0), nil
	}
	q, _ := strconv.Atoi(m[3])
	from = time.Date(year, time.Month(3*q-2), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 3, 0), nil
}

func monthPeriod(t time.Time) string { return t.UTC().Format("2006-01") }
func quarterPeriod(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())+2)/3)
}

// previousPeriods are the month and the quarter before the ones t is in.
func previousPeriods(t time.Time) (month, quarter string) {
	t = t.UTC()
	monthStart := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	quarterStart := time.Date(t.Year(), time.Month(3*((int(t.Month())-1)/3)+1), 1, 0, 0, 0, 0, time.UTC)
	return monthPeriod(monthStart.Add(-time.Hour)), quarterPeriod(quarterStart.Add(-time.Hour))
}

// sloReport returns the stored report of a closed period, or works one
// out: and stores it, when the period is closed.
func (s *Server) sloReport(ctx context.Context, period string, now time.Time) (*SLOReport, error) {
	from, to, err := parsePeriod(period)
	if err != nil {
		return nil, err
	}
	if !from.Before(now) {
		return nil, fmt.Errorf("period %s has not started", period)
	}
	if stored, err := s.store.SLOReport(defaultTenant, period); err != nil || stored != nil {
		return stored, err
	}
	rep, err := s.makeSLOReport(ctx, period, from, to, now)
	// A report without SLOs is not kept: it may come from a store that
	// answered with nothing for a while, and would stay empty for good.
	if err != nil || !rep.Final || len(rep.SLOs) == 0 {
		return rep, err
	}
	// Stored once: when two requests make it at the same time, the first
	// one stored is the one both answer with.
	if err := s.store.PutSLOReport(defaultTenant, rep); err != nil {
		return nil, err
	}
	return s.store.SLOReport(defaultTenant, period)
}

func (s *Server) makeSLOReport(ctx context.Context, period string, from, to, now time.Time) (*SLOReport, error) {
	rep := &SLOReport{Period: period, From: from, To: to, Created: now.UTC(), SLOs: []SLOReportRow{}}
	rep.Final = !now.Before(to.Add(reportSettle))
	at := to
	if !rep.Final {
		at = now.Add(-slo.ReadOffset)
	}
	rows, err := s.store.SLOs(defaultTenant)
	if err != nil || len(rows) == 0 {
		return rep, err
	}
	span := max(at.Sub(from).Truncate(time.Minute), time.Minute)
	w := model.Duration(span).String()
	query := func(expr string) (map[string]float64, error) {
		series, err := ruleBackend{s}.Query(ctx, "", expr, at)
		if err != nil {
			return nil, err
		}
		out := map[string]float64{}
		for _, sr := range series {
			out[sr.Labels["slo"]] = sr.Value
		}
		return out, nil
	}
	results := map[string]map[string]float64{}
	for key, expr := range map[string]string{
		"good":    fmt.Sprintf(`sum by (slo) (sum_over_time(%s[%s]))`, slo.GoodMetric, w),
		"total":   fmt.Sprintf(`sum by (slo) (sum_over_time(%s[%s]))`, slo.TotalMetric, w),
		"minutes": fmt.Sprintf(`sum by (slo) (count_over_time(%s[%s]))`, slo.TotalMetric, w),
		"first":   fmt.Sprintf(`min by (slo) (tfirst_over_time(%s[%s]))`, slo.TotalMetric, w),
		"objMin":  fmt.Sprintf(`min by (slo) (min_over_time(%s[%s]))`, slo.ObjectiveMetric, w),
		"objMax":  fmt.Sprintf(`max by (slo) (max_over_time(%s[%s]))`, slo.ObjectiveMetric, w),
	} {
		if results[key], err = query(expr); err != nil {
			return nil, err
		}
	}

	// Days that ended in the period, each its own sum: at midnight UTC,
	// a day's minutes are those stamped in it.
	days := map[string]map[time.Time][2]float64{} // slo -> day end -> good, total
	if lastDay := at.Truncate(24 * time.Hour); lastDay.After(from) {
		for i, metric := range []string{slo.GoodMetric, slo.TotalMetric} {
			series, err := s.queryRange(ctx, fmt.Sprintf(`sum by (slo) (sum_over_time(%s[1d]))`, metric), from.Add(24*time.Hour), lastDay, 24*time.Hour)
			if err != nil {
				return nil, err
			}
			for _, sr := range series {
				name := sr.Labels["slo"]
				if days[name] == nil {
					days[name] = map[time.Time][2]float64{}
				}
				for _, p := range sr.Points {
					v := days[name][p.T]
					v[i] = p.V
					days[name][p.T] = v
				}
			}
		}
	}

	for _, row := range rows {
		// An SLO that counted nothing in the period (made after it, or
		// broken all through) has nothing to report.
		if row.Error != "" || results["minutes"][row.Name] == 0 {
			continue
		}
		def := row.SLO
		r := SLOReportRow{Name: def.Name, Service: def.Service, Owner: def.Owner, Description: def.Description, Kind: def.Kind(),
			Objective: def.Objective, Good: results["good"][def.Name], Total: results["total"][def.Name], Incidents: []SLOReportIncident{}}
		lo, hasLo := results["objMin"][def.Name]
		hi := results["objMax"][def.Name]
		r.ObjectiveChanged = hasLo && math.Abs(hi-lo) > 1e-9
		r.Bad = math.Max(0, r.Total-r.Good)
		if r.Total > 0 {
			achieved := 100 * r.Good / r.Total
			met := achieved >= def.Objective-1e-9
			used := (r.Bad / r.Total) / def.Budget()
			r.Achieved, r.Met, r.BudgetUsed = &achieved, &met, &used
		}
		r.Coverage = math.Min(1, results["minutes"][def.Name]/span.Minutes())
		if first, ok := results["first"][def.Name]; ok && first > 0 {
			// The first minute is stamped at its end.
			if t := time.Unix(int64(first), 0).UTC().Add(-time.Minute); t.After(from) {
				r.Counting = &t
			}
		}
		var worst *SLOReportDay
		for end, v := range days[def.Name] {
			if v[1] <= 0 {
				continue
			}
			d := SLOReportDay{Day: end.Add(-24 * time.Hour).UTC().Format("2006-01-02"), Achieved: 100 * v[0] / v[1], Bad: math.Max(0, v[1]-v[0])}
			if worst == nil || d.Achieved < worst.Achieved || (d.Achieved == worst.Achieved && d.Day < worst.Day) {
				worst = &d
			}
		}
		if worst != nil && worst.Bad > 0 {
			r.WorstDay = worst
		}
		list, err := s.incidents.OfResource(ctx, "slo:"+def.Name, from, 200)
		if err != nil {
			return nil, err
		}
		for _, in := range list {
			if !in.FirstSeen.Before(to) {
				continue
			}
			end := at
			if !in.ResolvedAt.IsZero() && in.ResolvedAt.Before(end) {
				end = in.ResolvedAt
			}
			start := in.FirstSeen
			if start.Before(from) {
				start = from
			}
			r.Incidents = append(r.Incidents, SLOReportIncident{Alert: in.Labels["alertname"], Severity: in.Severity, Title: in.Title,
				Opened: in.FirstSeen, Resolved: in.ResolvedAt, Seconds: math.Max(0, end.Sub(start).Seconds()), AckBy: in.AckBy, AckComment: in.AckComment})
		}
		sort.Slice(r.Incidents, func(i, j int) bool { return r.Incidents[i].Opened.Before(r.Incidents[j].Opened) })
		rep.SLOs = append(rep.SLOs, r)
	}
	return rep, nil
}

// sloReportLoop stores the report of each month and quarter soon after it
// ends, without waiting for someone to ask: the incidents and the counts
// it is made from are not kept for ever.
func (s *Server) sloReportLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		if rows, err := s.store.SLOs(defaultTenant); err == nil && len(rows) > 0 {
			month, quarter := previousPeriods(now.Add(-reportSettle))
			for _, p := range []string{month, quarter} {
				// Only periods that ended within the settle time and a few days:
				// a server first started long after a period does not make up
				// a report from what is left of it.
				_, to, _ := parsePeriod(p)
				if now.Before(to.Add(reportSettle)) || now.After(to.Add(7*24*time.Hour)) {
					continue
				}
				if _, err := s.sloReport(ctx, p, now); err != nil && ctx.Err() == nil {
					s.log.Warn("SLO report not stored", "period", p, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sloReportRoutes(mux *http.ServeMux) {
	// GET /api/v1/slos/report?period=2026-10[&format=csv]; the period is
	// last month when none is given.
	mux.HandleFunc("GET /api/v1/slos/report", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		period := r.URL.Query().Get("period")
		if period == "" {
			period, _ = previousPeriods(now)
		}
		rep, err := s.sloReport(r.Context(), period, now)
		if err != nil {
			code := http.StatusBadGateway
			if strings.HasPrefix(err.Error(), "period ") {
				code = http.StatusBadRequest
			}
			http.Error(w, err.Error(), code)
			return
		}
		if r.URL.Query().Get("format") == "csv" {
			writeReportCSV(w, rep)
			return
		}
		writeJSON(w, rep)
	}))
	mux.HandleFunc("GET /api/v1/slos/reports", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.store.SLOReports(defaultTenant)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
}

func writeReportCSV(w http.ResponseWriter, rep *SLOReport) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="slo-report-%s.csv"`, rep.Period))
	c := csv.NewWriter(w)
	c.Write([]string{"period", "final", "slo", "service", "owner", "kind", "objective_percent", "achieved_percent", "met",
		"good", "total", "bad", "budget_used", "worst_day", "worst_day_achieved_percent", "coverage", "counting_since", "incidents", "incident_seconds"})
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	opt := func(v *float64) string {
		if v == nil {
			return ""
		}
		return num(*v)
	}
	for _, r := range rep.SLOs {
		met, worst, worstAch, since := "", "", "", ""
		if r.Met != nil {
			met = strconv.FormatBool(*r.Met)
		}
		if r.WorstDay != nil {
			worst, worstAch = r.WorstDay.Day, num(r.WorstDay.Achieved)
		}
		if r.Counting != nil {
			since = r.Counting.Format(time.RFC3339)
		}
		secs := 0.0
		for _, in := range r.Incidents {
			secs += in.Seconds
		}
		c.Write([]string{rep.Period, strconv.FormatBool(rep.Final), r.Name, r.Service, r.Owner, r.Kind, num(r.Objective), opt(r.Achieved), met,
			num(r.Good), num(r.Total), num(r.Bad), opt(r.BudgetUsed), worst, worstAch, num(r.Coverage), since,
			strconv.Itoa(len(r.Incidents)), num(math.Round(secs))})
	}
	c.Flush()
}

// SLOReport returns a stored report, or nil.
func (s *Store) SLOReport(tenant, period string) (*SLOReport, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM slo_reports WHERE tenant = ? AND period = ?`, tenant, period).Scan(&doc)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rep := &SLOReport{}
	return rep, json.Unmarshal([]byte(doc), rep)
}

// PutSLOReport stores a report unless one is stored for its period.
func (s *Store) PutSLOReport(tenant string, rep *SLOReport) error {
	doc, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO slo_reports (tenant, period, created, doc) VALUES (?, ?, ?, ?)`,
		tenant, rep.Period, rep.Created.Unix(), string(doc))
	return err
}

type storedReport struct {
	Period  string    `json:"period"`
	Created time.Time `json:"created"`
}

func (s *Store) SLOReports(tenant string) ([]storedReport, error) {
	rows, err := s.db.Query(`SELECT period, created FROM slo_reports WHERE tenant = ? ORDER BY period DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []storedReport{}
	for rows.Next() {
		var r storedReport
		var at int64
		if err := rows.Scan(&r.Period, &at); err != nil {
			return nil, err
		}
		r.Created = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
