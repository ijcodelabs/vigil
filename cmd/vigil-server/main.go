// Command vigil-server is the central node: it receives what agents send,
// stores it, evaluates rules and serves the interface and the API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ijcodelabs/vigil/internal/cli/servercli"
	"github.com/ijcodelabs/vigil/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}
	fs := flag.NewFlagSet("vigil-server", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to the server's YAML config file (default /etc/vigil/server.yaml)")
	fs.Parse(os.Args[1:])
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := servercli.Server(ctx, *cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
