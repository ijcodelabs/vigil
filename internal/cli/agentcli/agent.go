// Package agentcli holds the commands of vigil-agent: the agent itself and
// the tools that run on the agent's host. It must not import server code;
// cmd/vigil-agent has a test that checks.
package agentcli

import (
	"context"
	"log/slog"
	"os"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/version"
)

// Agent runs the agent until ctx ends.
func Agent(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/agent.yaml"
	}
	cfg, err := config.LoadAgent(path)
	if err != nil {
		return err
	}
	a, err := agent.New(cfg, version.Version, log)
	if err != nil {
		return err
	}
	log.Info("agent starting", "id", cfg.AgentID, "server", cfg.Server.URL, "version", version.Version)
	a.Run(ctx)
	return nil
}

// Events prints what the kernel probes report. With a config file it uses
// that file's security section (watch paths and rules); without one it
// watches a few sensitive paths and has no rules.
func Events(ctx context.Context, path string, log *slog.Logger) error {
	sec := &config.Security{WatchPaths: []string{"/etc/shadow", "/etc/sudoers", "/etc/passwd", "/root/.ssh/"}}
	if path != "" {
		cfg, err := config.LoadAgent(path)
		if err != nil {
			return err
		}
		sec = &cfg.Security
	} else {
		c, _ := config.ParseCollection("")
		c.Security.WatchPaths = sec.WatchPaths
		sec = &c.Security
	}
	return events.Dump(ctx, sec, os.Stdout, log)
}
