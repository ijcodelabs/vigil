// Package relaycli holds the command of vigil-relay. Like the agent's, it
// must not import server code; cmd/vigil-relay has a test that checks.
package relaycli

import (
	"context"
	"log/slog"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/relay"
	"github.com/ijcodelabs/vigil/internal/version"
)

// Relay runs the relay until ctx ends.
func Relay(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/relay.yaml"
	}
	cfg, err := config.LoadRelay(path)
	if err != nil {
		return err
	}
	r, err := relay.New(cfg, version.Version, log)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}
