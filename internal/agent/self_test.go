package agent

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
)

func TestSelfMetrics(t *testing.T) {
	cfg := &config.Agent{AgentID: "a", SpoolDir: t.TempDir(), SpoolMaxBytes: 1 << 20}
	cfg.Server.URL, cfg.Server.Token = "http://127.0.0.1:1", "t"
	a, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for i, x := 0, 0.0; i < 5e7; i++ { // use some processor time
		x += float64(i)
	}
	got := map[string]float64{}
	samples, _ := a.selfMetrics(context.Background())
	for _, s := range samples {
		got[s.Name] = s.Value
	}
	if got["vigil_agent_cpu_seconds_total"] <= 0 || got["vigil_agent_memory_bytes"] < 1<<20 ||
		got["vigil_agent_goroutines"] < 1 || got["vigil_agent_start_time_seconds"] < 1.7e9 {
		t.Errorf("self metrics: %v", got)
	}
	if _, ok := got["vigil_agent_open_files"]; ok != (runtime.GOOS == "linux") {
		t.Errorf("open files reported: %v on %s", ok, runtime.GOOS)
	}
}
