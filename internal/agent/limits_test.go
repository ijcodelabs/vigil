package agent

import (
	"fmt"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

func TestSeriesLimit(t *testing.T) {
	var l limiter
	l.configure(config.Limits{MaxSeries: 100, MaxLogBytesPerSecond: 1})
	batch := func(n int) []model.Sample {
		out := make([]model.Sample, n)
		for i := range out {
			out[i] = model.Sample{Name: "requests_total", Labels: map[string]string{"path": fmt.Sprint("/p", i), "method": "GET"}}
		}
		return out
	}
	if got := l.samples(batch(150)); len(got) != 100 || l.droppedSeries.Load() != 50 {
		t.Fatalf("kept %d, dropped %d", len(got), l.droppedSeries.Load())
	}
	// The series already admitted keep flowing; only new ones are refused.
	if got := l.samples(batch(150)); len(got) != 100 || l.droppedSeries.Load() != 100 {
		t.Errorf("second round: kept %d, dropped %d", len(got), l.droppedSeries.Load())
	}
	// The order of labels in the map must not make one series look like two.
	a := model.Sample{Name: "x", Labels: map[string]string{"a": "1", "b": "2", "c": "3"}}
	b := model.Sample{Name: "x", Labels: map[string]string{"c": "3", "b": "2", "a": "1"}}
	c := model.Sample{Name: "x", Labels: map[string]string{"a": "2", "b": "1", "c": "3"}}
	if seriesID(&a) != seriesID(&b) || seriesID(&a) == seriesID(&c) {
		t.Error("series identity")
	}
}

func TestLogRateLimit(t *testing.T) {
	var l limiter
	l.configure(config.Limits{MaxSeries: 1, MaxLogBytesPerSecond: 100}) // a burst of one minute: 6000 bytes
	line := model.LogEntry{Msg: string(make([]byte, 936))}              // counted as 1000 bytes
	batch := make([]model.LogEntry, 10)
	for i := range batch {
		batch[i] = line
	}
	if got := l.logs(batch); len(got) != 6 || l.droppedLogs.Load() != 4000 {
		t.Errorf("kept %d lines, dropped %d bytes", len(got), l.droppedLogs.Load())
	}
}
