package systemd

import (
	"context"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
)

const listing = `auditd.service                 loaded    active   running Security Audit Logging Service
chronyd.service                loaded    active   running NTP client/server
backup.service                 loaded    failed   failed  Nightly backup
nginx.service                  loaded    inactive dead    A high performance web server
display-manager.service        not-found inactive dead    display-manager.service
session-3.scope                loaded    active   running Session 3
`

func TestCollect(t *testing.T) {
	c := New(config.Systemd{Enabled: true, Units: []string{"nginx.service", "back*"}})
	c.Run = func(context.Context, ...string) ([]byte, error) { return []byte(listing), nil }
	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		got[s.Name+"/"+s.Labels["unit"]+"/"+s.Labels["state"]] = s.Value
	}
	want := map[string]float64{"systemd_units//active": 2, "systemd_units//failed": 1, "systemd_units//inactive": 1,
		"systemd_unit_state/nginx.service/inactive": 1, "systemd_unit_state/backup.service/failed": 1}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
