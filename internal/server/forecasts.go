package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ijcodelabs/vigil/internal/forecast"
	"github.com/ijcodelabs/vigil/internal/rules"
)

// What will become a problem, for the Home page: only cases whose
// mechanism is plain and whose error can be shown. Worked out every ten
// minutes in the background; the briefing reads the last result.

const forecastInterval = 10 * time.Minute

// Coming is one thing that will need attention.
type Coming struct {
	Kind     string  `json:"kind"` // disk_full, cert_expiry, agent_limited
	Agent    string  `json:"agent,omitempty"`
	Resource string  `json:"resource"`
	Title    string  `json:"title"`
	Severity string  `json:"severity"` // warning (within a week) or info
	Soonest  float64 `json:"soonest_s,omitempty"`
	Latest   float64 `json:"latest_s,omitempty"`
	// Query shows the data the forecast is made from, for a chart.
	Query string `json:"query,omitempty"`
}

type comingState struct {
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
	Items []Coming  `json:"items"`
}

var diskRules = forecast.Rules{MinHistory: 72 * time.Hour, Short: 24 * time.Hour, MaxRatio: 3, Horizon: 30 * 24 * time.Hour}

const diskSelector = `host_filesystem_free_bytes{fstype!~"tmpfs|devtmpfs|overlay|squashfs|ramfs|nsfs|autofs|proc|sysfs"}`

func (s *Server) forecastLoop(ctx context.Context) {
	t := time.NewTicker(forecastInterval)
	defer t.Stop()
	for {
		st := s.forecasts(ctx, time.Now())
		if st.Error != "" && ctx.Err() == nil {
			if prev := s.comingState.Load(); prev == nil || prev.Error == "" {
				s.log.Warn("forecasts are not being made", "err", st.Error)
			}
		}
		s.comingState.Store(&st)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) forecasts(ctx context.Context, now time.Time) comingState {
	st := comingState{At: now, Items: []Coming{}}
	var errs []string
	for _, f := range []func(context.Context, time.Time) ([]Coming, error){s.diskForecasts, s.certForecasts, s.limitedAgents} {
		items, err := f(ctx, now)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		st.Items = append(st.Items, items...)
	}
	if len(errs) > 0 {
		st.Error = fmt.Sprint(errs)
	}
	sort.SliceStable(st.Items, func(i, j int) bool {
		a, b := st.Items[i], st.Items[j]
		if a.Severity != b.Severity {
			return a.Severity == "warning"
		}
		return a.Soonest < b.Soonest
	})
	return st
}

// diskForecasts: when each filesystem will be full, from up to a week of
// free space. Filesystems with fewer than three days of clean data, that
// are not filling, or whose last day disagrees with the week get none.
func (s *Server) diskForecasts(ctx context.Context, now time.Time) ([]Coming, error) {
	free, err := s.queryRange(ctx, diskSelector, now.Add(-7*24*time.Hour), now, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	sizes, err := ruleBackend{s}.Query(ctx, "", `host_filesystem_size_bytes`, now)
	if err != nil {
		return nil, err
	}
	size := map[string]float64{}
	key := func(l map[string]string) string { return l["agent"] + "\x00" + l["mountpoint"] + "\x00" + l["device"] }
	for _, sr := range sizes {
		size[key(sr.Labels)] = sr.Value
	}
	var out []Coming
	for _, sr := range free {
		total := size[key(sr.Labels)]
		if total < 1e9 || len(sr.Points) == 0 {
			continue // small or unknown: not worth a line
		}
		r := diskRules
		r.ResetJump = max(0.02*total, 256<<20)
		res, ok := forecast.TimeToEmpty(sr.Points, r)
		if !ok {
			continue
		}
		left := sr.Points[len(sr.Points)-1].V
		agent, mount := sr.Labels["agent"], sr.Labels["mountpoint"]
		c := Coming{Kind: "disk_full", Agent: agent, Resource: "host:" + agent, Severity: "info",
			Soonest: res.Soonest.Seconds(), Latest: res.Latest.Seconds(),
			Title: fmt.Sprintf("%s %s will be full in %s (%.0f%% used, %s a day)", agent, mount,
				dayRange(res.Soonest, res.Latest), 100*(1-left/total), bytesText(res.PerDay)),
			Query: fmt.Sprintf(`100 * (1 - host_filesystem_free_bytes{agent=%q, mountpoint=%q} / host_filesystem_size_bytes)`, agent, mount)}
		if res.Soonest <= 7*24*time.Hour {
			c.Severity = "warning"
		}
		out = append(out, c)
	}
	return out, nil
}

// certForecasts: certificates that expire within 30 days but not yet
// within 14, where the built-in CertificateExpiresSoon alert takes over.
func (s *Server) certForecasts(ctx context.Context, now time.Time) ([]Coming, error) {
	series, err := ruleBackend{s}.Query(ctx, "", `probe_tls_cert_expiry_seconds > 14 * 86400 < 30 * 86400`, now)
	if err != nil {
		return nil, err
	}
	var out []Coming
	for _, sr := range series {
		l := sr.Labels
		left := time.Duration(sr.Value * float64(time.Second))
		out = append(out, Coming{Kind: "cert_expiry", Agent: l["agent"], Resource: "check:" + l["agent"] + "/" + l["probe"],
			Severity: "info", Soonest: left.Seconds(), Latest: left.Seconds(),
			Title: fmt.Sprintf("The certificate of %s expires in %d days (check %s)", l["target"], int(left.Hours()/24), l["probe"]),
			Query: fmt.Sprintf(`probe_tls_cert_expiry_seconds{probe=%q} / 86400`, l["probe"])})
	}
	return out, nil
}

// limitedAgents: agents the server held back in the last hour because they
// sent more than their limits allow; they drop data next.
func (s *Server) limitedAgents(ctx context.Context, now time.Time) ([]Coming, error) {
	series, err := ruleBackend{s}.Query(ctx, "", `sum by (agent) (increase(vigil_agent_limited_batches_total[1h])) > 0`, now)
	if err != nil {
		return nil, err
	}
	var out []Coming
	for _, sr := range series {
		a := sr.Labels["agent"]
		out = append(out, Coming{Kind: "agent_limited", Agent: a, Resource: "host:" + a, Severity: "warning",
			Title: fmt.Sprintf("Agent %s was held back by its limits %.0f times in the last hour; it drops data when its spool fills", a, sr.Value),
			Query: fmt.Sprintf(`increase(vigil_agent_limited_batches_total{agent=%q}[1h])`, a)})
	}
	return out, nil
}

func dayRange(a, b time.Duration) string {
	da, db := int(math.Round(a.Hours()/24)), int(math.Round(b.Hours()/24))
	switch {
	case db < 1:
		return "less than a day"
	case da == db:
		return fmt.Sprintf("about %d days", da)
	default:
		return fmt.Sprintf("%d to %d days", max(da, 1), db)
	}
}

func bytesText(v float64) string {
	for _, u := range []struct {
		n float64
		s string
	}{{1 << 40, "TB"}, {1 << 30, "GB"}, {1 << 20, "MB"}} {
		if v >= u.n {
			return fmt.Sprintf("%.1f %s", v/u.n, u.s)
		}
	}
	return fmt.Sprintf("%.0f kB", v/1024)
}

type rangeSeries struct {
	Labels map[string]string
	Points []forecast.Point
}

// queryRange asks the metric store for samples over a time range.
func (s *Server) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]rangeSeries, error) {
	u := s.metrics.JoinPath("/api/v1/query_range")
	q := u.Query()
	q.Set("query", expr)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, &rules.QueryError{Msg: "store unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("range query refused (HTTP %d): %.300s", resp.StatusCode, body)
	}
	var res struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("unreadable range response: %w", err)
	}
	out := make([]rangeSeries, 0, len(res.Data.Result))
	for _, r := range res.Data.Result {
		sr := rangeSeries{Labels: r.Metric}
		for _, v := range r.Values {
			ts, _ := v[0].(float64)
			str, _ := v[1].(string)
			if f, err := strconv.ParseFloat(str, 64); err == nil {
				sr.Points = append(sr.Points, forecast.Point{T: time.Unix(int64(ts), 0), V: f})
			}
		}
		out = append(out, sr)
	}
	return out, nil
}

// Spend is AI spending for the briefing, from ai_cost_usd_per_minute: the
// last 24 hours against the daily average of the week before.
type Spend struct {
	At           time.Time `json:"at"`
	Billed       float64   `json:"billed"`
	Subscription float64   `json:"subscription"` // at API prices, not billed
	UsualBilled  float64   `json:"usual_billed"` // a day, averaged over the 7 days before
	Unpriced     int       `json:"unpriced"`     // models and GPU kinds in use without a price
}

// spend returns nil when nothing writes the cost metric.
func (s *Server) spend(ctx context.Context, now time.Time) (*Spend, error) {
	b := ruleBackend{s}
	one := func(expr string) (float64, bool, error) {
		series, err := b.Query(ctx, "", expr, now)
		if err != nil || len(series) == 0 {
			return 0, false, err
		}
		return series[0].Value, true, nil
	}
	if _, any, err := one(`count(ai_cost_usd_per_minute)`); err != nil || !any {
		return nil, err
	}
	sp := &Spend{At: now}
	var err error
	for _, q := range []struct {
		dst  *float64
		expr string
	}{
		{&sp.Billed, `sum(sum_over_time(ai_cost_usd_per_minute{billing!="subscription"}[1d]))`},
		{&sp.Subscription, `sum(sum_over_time(ai_cost_usd_per_minute{billing="subscription"}[1d]))`},
		{&sp.UsualBilled, `sum(sum_over_time(ai_cost_usd_per_minute{billing!="subscription"}[7d] offset 1d)) / 7`},
	} {
		if *q.dst, _, err = one(q.expr); err != nil {
			return nil, err
		}
	}
	unpriced, _, err := one(`(count(count by (model) (genai_tokens_total) unless on (model) genai_token_price) or vector(0))
		+ (count(count by (model) (gpu_info) unless on (model) gpu_hour_price) or vector(0))`)
	sp.Unpriced = int(unpriced)
	return sp, err
}

func (s *Server) spendLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if sp, err := s.spend(ctx, time.Now()); err == nil {
			s.spendState.Store(sp)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
