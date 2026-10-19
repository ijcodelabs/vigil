// Command vigil is both the host agent and the central server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/otlp"
	"github.com/ijcodelabs/vigil/internal/relay"
	"github.com/ijcodelabs/vigil/internal/server"
)

var version = "dev" // set with -ldflags "-X main.version=..."

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  vigil agent  -config /etc/vigil/agent.yaml
  vigil server -config /etc/vigil/server.yaml
  vigil relay  -config /etc/vigil/relay.yaml     pass on what agents send to the server
  vigil tracegen [-endpoint http://127.0.0.1:4318] [-rate 2]   send demo traces to an agent's OTLP endpoint
  vigil claude-hook [-endpoint http://127.0.0.1:4318] [-content]   a Claude Code hook: one audit line per call to the agent
  vigil events [-config /etc/vigil/agent.yaml]   print runtime events as they happen (Linux, root)
  vigil apply -f DIR -server URL [-prune] [-dry-run] [-immediate]   make the server's profiles match a directory
  vigil enroll -server URL -token T [-ca-pin sha256:...] [-id NAME] [-label k=v]   enrol this host and write its agent config
  vigil version`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to the YAML config file")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if os.Args[1] == "claude-hook" {
		runClaudeHook(os.Args[2:]) // never fails; see its comment
		return
	}

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
		return
	case "agent":
		fs.Parse(os.Args[2:])
		err = runAgent(ctx, *cfgPath, log)
	case "server":
		fs.Parse(os.Args[2:])
		err = runServer(ctx, *cfgPath, log)
	case "apply":
		err = runApply(os.Args[2:])
	case "relay":
		fs.Parse(os.Args[2:])
		err = runRelay(ctx, *cfgPath, log)
	case "tracegen":
		endpoint := fs.String("endpoint", "http://127.0.0.1:4318", "an agent's OTLP endpoint")
		rate := fs.Float64("rate", 2, "traces a second")
		fs.Parse(os.Args[2:])
		fmt.Fprintf(os.Stderr, "sending demo traces of a small shop to %s, %.3g a second; Ctrl-C stops\n", *endpoint, *rate)
		var n int
		n, err = otlp.SendDemo(ctx, *endpoint, *rate)
		fmt.Fprintf(os.Stderr, "sent %d traces\n", n)
	case "enroll":
		err = runEnroll(ctx, os.Args[2:])
	case "events":
		fs.Parse(os.Args[2:])
		err = runEvents(ctx, *cfgPath, log)
	default:
		usage()
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runAgent(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/agent.yaml"
	}
	cfg, err := config.LoadAgent(path)
	if err != nil {
		return err
	}
	a, err := agent.New(cfg, version, log)
	if err != nil {
		return err
	}
	log.Info("agent starting", "id", cfg.AgentID, "server", cfg.Server.URL, "version", version)
	a.Run(ctx)
	return nil
}

func runServer(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/server.yaml"
	}
	cfg, err := config.LoadServer(path)
	if err != nil {
		return err
	}
	server.Version = version
	s, err := server.New(cfg, log)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Run(ctx)
}

func runRelay(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		path = "/etc/vigil/relay.yaml"
	}
	cfg, err := config.LoadRelay(path)
	if err != nil {
		return err
	}
	r, err := relay.New(cfg, version, log)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

// runEvents prints what the kernel probes report. With a config file it uses
// that file's security section (watch paths and rules); without one it
// watches a few sensitive paths and has no rules.
func runEvents(ctx context.Context, path string, log *slog.Logger) error {
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
