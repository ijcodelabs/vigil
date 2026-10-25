// Package servercli holds the command of vigil-server.
package servercli

import (
	"context"
	"log/slog"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
	"github.com/ijcodelabs/vigil/internal/version"
)

// Server runs the server until ctx ends.
func Server(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/server.yaml"
	}
	cfg, err := config.LoadServer(path)
	if err != nil {
		return err
	}
	server.Version = version.Version
	s, err := server.New(cfg, log)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Run(ctx)
}
