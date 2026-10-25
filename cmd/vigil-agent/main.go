// Command vigil-agent runs on every monitored host or Kubernetes node: the
// agent itself, and the tools that belong on that host.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ijcodelabs/vigil/internal/cli/agentcli"
	"github.com/ijcodelabs/vigil/internal/version"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  vigil-agent [-config /etc/vigil/agent.yaml]   run the agent
  vigil-agent enroll -server URL -token T [-ca-pin sha256:...] [-id NAME] [-label k=v]   enrol this host and write its agent config
  vigil-agent events [-config /etc/vigil/agent.yaml]   print runtime events as they happen (Linux, root)
  vigil-agent claude-hook [-endpoint http://127.0.0.1:4318] [-content]   a Claude Code hook: one audit line per call to the agent
  vigil-agent version`)
	os.Exit(2)
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "claude-hook" {
		agentcli.ClaudeHook(args[1:]) // never fails; see its comment
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := "agent"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	cfgPath := fs.String("config", "", "path to the agent's YAML config file")
	var err error
	switch cmd {
	case "version":
		fmt.Println(version.Version)
		return
	case "agent":
		fs.Parse(args)
		err = agentcli.Agent(ctx, *cfgPath, log)
	case "enroll":
		err = agentcli.Enroll(ctx, args)
	case "events":
		fs.Parse(args)
		err = agentcli.Events(ctx, *cfgPath, log)
	default:
		usage()
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
