// Command vigil holds the operator tools (apply, tracegen). For one release
// it is also still the agent, the server and the relay, so hosts installed
// with `vigil agent` keep working while they move to vigil-agent,
// vigil-server and vigil-relay, which are smaller and carry only their part.
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
	"github.com/ijcodelabs/vigil/internal/cli/opscli"
	"github.com/ijcodelabs/vigil/internal/cli/relaycli"
	"github.com/ijcodelabs/vigil/internal/cli/servercli"
	"github.com/ijcodelabs/vigil/internal/version"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  vigil apply -f DIR -server URL [-prune] [-dry-run] [-immediate]   make the server's profiles match a directory
  vigil tracegen [-endpoint http://127.0.0.1:4318] [-rate 2]   send demo traces to an agent's OTLP endpoint
  vigil version

still here for one release, moved to their own binaries:
  vigil agent, enroll, events, claude-hook   ->  vigil-agent
  vigil server                               ->  vigil-server
  vigil relay                                ->  vigil-relay`)
	os.Exit(2)
}

// moved names the binary a subcommand now lives in.
var moved = map[string]string{
	"agent": "vigil-agent -config ...", "enroll": "vigil-agent enroll", "events": "vigil-agent events",
	"server": "vigil-server -config ...", "relay": "vigil-relay -config ...",
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "claude-hook" {
		agentcli.ClaudeHook(os.Args[2:]) // never fails and prints nothing; see its comment
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if to, ok := moved[os.Args[1]]; ok {
		log.Warn("`vigil "+os.Args[1]+"` is going away after this release; use `"+to+"`", "version", version.Version)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to the YAML config file")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version.Version)
		return
	case "apply":
		err = opscli.Apply(os.Args[2:])
	case "tracegen":
		err = opscli.Tracegen(ctx, os.Args[2:])
	case "agent":
		fs.Parse(os.Args[2:])
		err = agentcli.Agent(ctx, *cfgPath, log)
	case "server":
		fs.Parse(os.Args[2:])
		err = servercli.Server(ctx, *cfgPath, log)
	case "relay":
		fs.Parse(os.Args[2:])
		err = relaycli.Relay(ctx, *cfgPath, log)
	case "enroll":
		err = agentcli.Enroll(ctx, os.Args[2:])
	case "events":
		fs.Parse(os.Args[2:])
		err = agentcli.Events(ctx, *cfgPath, log)
	default:
		usage()
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
