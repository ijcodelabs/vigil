package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	commonmodel "github.com/prometheus/common/model"

	"github.com/ijcodelabs/vigil/internal/rules"
)

// The month and year totals come with the date of their first sample: a
// store that keeps 30 days gives a "year" that starts 30 days ago.
func TestSpendPeriods(t *testing.T) {
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	answers := map[string]string{
		`{billing!="subscription"}[30d]`:  "12.5",
		`{billing="subscription"}[30d]`:   "40",
		`{billing!="subscription"}[365d]`: "12.5",
		`tfirst_over_time`:                fmt.Sprint(since.Unix()),
	}
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, rows := r.URL.Query().Get("query"), ""
		for k, v := range answers {
			if strings.Contains(q, k) {
				rows = `{"metric":{},"value":[1700000000,"` + v + `"]}`
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, rows)
	}))
	defer store.Close()
	u, _ := url.Parse(store.URL)
	s := &Server{metrics: u, http: store.Client()}

	got, err := s.spendPeriods(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []SpendPeriod{{Days: 30, Billed: 12.5, Subscription: 40, Since: since}, {Days: 365, Billed: 12.5, Since: since}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("periods:\n got %v\nwant %v", got, want)
	}
}

// With a history store that reaches further back, a period is its hourly
// sums plus the main store's minutes since the last hour it has.
func TestSpendPeriodsFromHistory(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 20, 0, 0, time.UTC)
	mainSince, histFirst, histLast := now.Add(-20*24*time.Hour), now.Add(-200*24*time.Hour), now.Add(-20*time.Minute)
	fake := func(answers map[string]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q, rows := r.URL.Query().Get("query"), ""
			for k, v := range answers {
				if strings.Contains(q, k) {
					rows = `{"metric":{},"value":[1700000000,"` + v + `"]}`
				}
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, rows)
		}))
	}
	mainStore := fake(map[string]string{
		`{billing!="subscription"}[30d]`:   "3",
		`{billing!="subscription"}[365d]`:  "3",
		`{billing!="subscription"}[1200s]`: "0.5", // the 20 minutes after the last rollup
		`tfirst_over_time`:                 fmt.Sprint(mainSince.Unix()),
	})
	defer mainStore.Close()
	hist := fake(map[string]string{
		`ai_cost_usd_per_hour{billing!="subscription"}[365d]`: "100",
		`ai_cost_usd_per_hour{billing!="subscription"}[30d]`:  "10",
		`tfirst_over_time`: fmt.Sprint(histFirst.Unix()),
		`tlast_over_time`:  fmt.Sprint(histLast.Unix()),
	})
	defer hist.Close()
	mu, _ := url.Parse(mainStore.URL)
	hu, _ := url.Parse(hist.URL)
	s := &Server{metrics: mu, history: hu, http: mainStore.Client(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got, err := s.spendPeriods(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	since := histFirst.Add(-time.Hour)
	want := []SpendPeriod{{Days: 30, Billed: 10.5, Since: since}, {Days: 365, Billed: 100.5, Since: since}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("periods:\n got %v\nwant %v", got, want)
	}
}

// The rollups parse, and each becomes an hourly group that writes to the
// history store and fills in the last 90 days on its first run.
func TestHistoryRuleGroups(t *testing.T) {
	groups := historyRuleGroups()
	if len(groups) == 0 {
		t.Fatal("no history groups")
	}
	for _, g := range groups {
		alignHistory(&g)
		if g.Store != rules.StoreHistory || g.Delay != historyDelay || g.Backfill != historyBackfill {
			t.Errorf("%s: store %q, delay %v, backfill %v", g.Name, g.Store, g.Delay, g.Backfill)
		}
	}
	g := rules.Group{Store: rules.StoreHistory, Interval: commonmodel.Duration(time.Minute)}
	if alignHistory(&g); g.Backfill != historyBackfillEvals*time.Minute {
		t.Errorf("a 1-minute group backfills %v", g.Backfill)
	}
}
