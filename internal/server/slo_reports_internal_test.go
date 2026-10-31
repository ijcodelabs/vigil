package server

import (
	"testing"
	"time"
)

func TestReportPeriods(t *testing.T) {
	for _, c := range []struct{ p, from, to string }{
		{"2026-10", "2026-10-01", "2026-11-01"},
		{"2026-12", "2026-12-01", "2027-01-01"},
		{"2026-Q4", "2026-10-01", "2027-01-01"},
		{"2026-Q1", "2026-01-01", "2026-04-01"},
	} {
		from, to, err := parsePeriod(c.p)
		if err != nil || from.Format(time.DateOnly) != c.from || to.Format(time.DateOnly) != c.to {
			t.Errorf("%s: %s to %s, %v", c.p, from, to, err)
		}
	}
	for _, bad := range []string{"2026-13", "2026-Q5", "2026-1", "26-10", "2026-10-01"} {
		if _, _, err := parsePeriod(bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
	for _, c := range []struct{ at, month, quarter string }{
		{"2026-03-31T12:00:00Z", "2026-02", "2025-Q4"},
		{"2026-04-01T00:05:00Z", "2026-03", "2026-Q1"},
		{"2027-01-15T00:00:00Z", "2026-12", "2026-Q4"},
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		if m, q := previousPeriods(at); m != c.month || q != c.quarter {
			t.Errorf("%s: %s %s", c.at, m, q)
		}
	}
}
