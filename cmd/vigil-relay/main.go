// Command vigil-relay passes on what agents send to the server, for sites
// whose agents cannot reach the server directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ijcodelabs/vigil/internal/cli/relaycli"
	"github.com/ijcodelabs/vigil/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}
	fs := flag.NewFlagSet("vigil-relay", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to the relay's YAML config file (default /etc/vigil/relay.yaml)")
	fs.Parse(os.Args[1:])
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := relaycli.Relay(ctx, *cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
