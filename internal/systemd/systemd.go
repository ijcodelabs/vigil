// Package systemd reports the state of systemd service units by running
// systemctl, which keeps the agent free of a D-Bus dependency.
package systemd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

// ErrUnavailable means there is no systemctl to ask (a container, a host
// without systemd).
var ErrUnavailable = errors.New("systemctl not found")

type Collector struct {
	units []string
	// Run executes systemctl and returns its output; replaceable in tests.
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

func New(cfg config.Systemd) *Collector {
	return &Collector{units: cfg.Units, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		if _, err := exec.LookPath("systemctl"); err != nil {
			return nil, ErrUnavailable
		}
		return exec.CommandContext(ctx, "systemctl", args...).Output()
	}}
}

// Collect returns:
//
//	systemd_units{state}             how many service units are in each state
//	systemd_unit_state{unit,state}   1 for the current state of each unit matching the configured patterns
//
// A unit that failed is in state "failed"; that is the one to alert on.
func (c *Collector) Collect(ctx context.Context) ([]model.Sample, error) {
	out, err := c.Run(ctx, "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		return nil, err
	}
	ts := time.Now().UnixMilli()
	counts := map[string]float64{"active": 0, "inactive": 0, "failed": 0}
	var samples []model.Sample
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text()) // UNIT LOAD ACTIVE SUB DESCRIPTION...
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		unit, load, state := f[0], f[1], f[2]
		if load == "not-found" { // referenced by another unit but not installed
			continue
		}
		counts[state]++
		for _, pattern := range c.units {
			if ok, _ := filepath.Match(pattern, unit); ok {
				samples = append(samples, model.Sample{Name: "systemd_unit_state",
					Labels: map[string]string{"unit": unit, "state": state}, Value: 1, TS: ts})
				break
			}
		}
	}
	for state, n := range counts {
		samples = append(samples, model.Sample{Name: "systemd_units", Labels: map[string]string{"state": state}, Value: n, TS: ts})
	}
	return samples, nil
}
