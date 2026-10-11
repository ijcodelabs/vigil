package procmetrics

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
)

func TestCollect(t *testing.T) {
	off := false
	self := filepath.Base(os.Args[0]) // the test binary itself is a process we know is running
	c := New([]config.ProcessGroup{
		{Name: "me", Cmdline: regexp.QuoteMeta(self)},
		{Name: "ghost", Match: "no-such-process-anywhere"},
		{Name: "off", Match: "x", Enabled: &off},
	})
	if !c.Active() {
		t.Fatal("not active")
	}
	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		got[s.Labels["group"]+"/"+s.Name] = s.Value
	}
	if got["me/process_group_count"] < 1 || got["me/process_group_memory_rss_bytes"] <= 0 || got["me/process_group_threads"] < 1 {
		t.Errorf("own process: %v", got)
	}
	// A group nothing matches is still reported, as zero: that is how "not running" is alerted on.
	if v, ok := got["ghost/process_group_count"]; !ok || v != 0 {
		t.Errorf("absent process: %v", got)
	}
	if _, ok := got["off/process_group_count"]; ok || len(samples) != 8 {
		t.Errorf("disabled group reported: %d samples", len(samples))
	}
}
