// Package forecast estimates when a resource runs out, conservatively:
// a forecast is only made when the data is long enough, clean enough and
// says the same thing over a short and a long window. A wrong prediction
// costs more trust than a missing one.
package forecast

import (
	"math"
	"sort"
	"time"
)

// Point is one sample of what is left (free bytes, say).
type Point struct {
	T time.Time
	V float64
}

// Rules for a forecast.
type Rules struct {
	MinHistory time.Duration // after the last reset
	Short      time.Duration // the recent window, whose slope must agree with the whole
	MaxRatio   float64       // the two times to empty may differ by this factor at most
	Horizon    time.Duration // farther than this is not worth saying
	// A lasting rise of more than this (in the unit of V) is a cleanup or a
	// resize: the forecast starts again from there.
	ResetJump float64
}

// Result says when it runs out: between Soonest and Latest from now.
type Result struct {
	Soonest, Latest time.Duration
	PerDay          float64 // how much is used up per day (long window)
	Since           time.Time
}

// TimeToEmpty forecasts when the values reach zero. ok is false when no
// forecast should be shown: too little or too uneven data, not falling, the
// windows disagree, or it is beyond the horizon.
func TimeToEmpty(points []Point, r Rules) (res Result, ok bool) {
	if len(points) < 2 {
		return res, false
	}
	sort.Slice(points, func(i, j int) bool { return points[i].T.Before(points[j].T) })
	// A cleanup is a rise that lasts: the samples after it sit higher than
	// those before. A spike that comes back down is not one.
	const around = 6
	start := 0
	for i := around; i+around <= len(points); i++ {
		if median(points[i:i+around])-median(points[i-around:i]) > r.ResetJump {
			start = i
		}
	}
	points = points[start:]
	if len(points) < 12 || points[len(points)-1].T.Sub(points[0].T) < r.MinHistory {
		return res, false
	}
	last := points[len(points)-1]
	var recent []Point
	for _, p := range points {
		if last.T.Sub(p.T) <= r.Short {
			recent = append(recent, p)
		}
	}
	if len(recent) < 6 {
		return res, false
	}
	long, short := theilSen(points), theilSen(recent)
	if long >= 0 || short >= 0 || last.V <= 0 {
		return res, false
	}
	tLong := time.Duration(last.V / -long * float64(time.Second))
	tShort := time.Duration(last.V / -short * float64(time.Second))
	soon, late := min(tLong, tShort), max(tLong, tShort)
	if float64(late) > r.MaxRatio*float64(soon) || soon > r.Horizon {
		return res, false
	}
	// From the last sample, not from now: the store is a little behind.
	return Result{Soonest: soon, Latest: late, PerDay: -long * 86400, Since: points[0].T}, true
}

// theilSen is the median of the slopes between all pairs of points (per
// second): one odd sample or a short burst barely moves it. Points are
// thinned to at most 200 first, so the pairs stay few.
func theilSen(points []Point) float64 {
	if n := len(points); n > 200 {
		thin := make([]Point, 0, 200)
		for i := 0; i < 200; i++ {
			thin = append(thin, points[i*(n-1)/199])
		}
		points = thin
	}
	var slopes []float64
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			if dt := points[j].T.Sub(points[i].T).Seconds(); dt > 0 {
				slopes = append(slopes, (points[j].V-points[i].V)/dt)
			}
		}
	}
	if len(slopes) == 0 {
		return math.NaN()
	}
	sort.Float64s(slopes)
	m := len(slopes) / 2
	if len(slopes)%2 == 1 {
		return slopes[m]
	}
	return (slopes[m-1] + slopes[m]) / 2
}

func median(points []Point) float64 {
	v := make([]float64, len(points))
	for i, p := range points {
		v[i] = p.V
	}
	sort.Float64s(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}
