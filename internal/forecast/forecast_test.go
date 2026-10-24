package forecast

import (
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rules = Rules{MinHistory: 72 * time.Hour, Short: 24 * time.Hour, MaxRatio: 3, Horizon: 30 * 24 * time.Hour, ResetJump: 5}
)

// series: free space over days, one sample an hour, using perDay a day.
func series(days int, start, perDay float64, f func(i int, v float64) float64) []Point {
	var out []Point
	for i := 0; i <= days*24; i++ {
		v := start - perDay*float64(i)/24
		if f != nil {
			v = f(i, v)
		}
		out = append(out, Point{T: t0.Add(time.Duration(i) * time.Hour), V: v})
	}
	return out
}

func TestSteadyFill(t *testing.T) {
	// 100 left, 10 a day for 4 days: 60 left, 6 days to go.
	res, ok := TimeToEmpty(series(4, 100, 10, nil), rules)
	if !ok || res.Soonest < 5*24*time.Hour || res.Latest > 7*24*time.Hour || res.PerDay < 9.9 || res.PerDay > 10.1 {
		t.Fatalf("steady: %v %+v", ok, res)
	}
}

func TestNoForecast(t *testing.T) {
	for name, pts := range map[string][]Point{
		"too short":      series(2, 100, 10, nil),
		"not falling":    series(4, 100, 0, nil),
		"rising":         series(4, 100, -5, nil),
		"beyond horizon": series(4, 1000, 1, nil),
		"windows disagree": series(4, 100, 1, func(i int, v float64) float64 { // flat for days, then a steep last day
			if i > 72 {
				return v - float64(i-72)*2
			}
			return v
		}),
		// A cleanup two days ago: only two days of data since.
		"reset by cleanup": series(4, 100, 10, func(i int, v float64) float64 {
			if i >= 48 {
				return v + 50
			}
			return v
		}),
	} {
		if res, ok := TimeToEmpty(pts, rules); ok {
			t.Errorf("%s: forecast %+v", name, res)
		}
	}
}

// A few wild samples do not move the robust slope much.
func TestOutliers(t *testing.T) {
	pts := series(4, 100, 10, func(i int, v float64) float64 {
		if i%17 == 0 {
			return v - 30 // a spike of temporary files
		}
		return v
	})
	res, ok := TimeToEmpty(pts, rules)
	if !ok || res.PerDay < 9 || res.PerDay > 11 {
		t.Fatalf("with outliers: %v %+v", ok, res)
	}
}
