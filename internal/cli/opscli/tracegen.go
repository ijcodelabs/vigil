// Package opscli holds the operator tools of the vigil command: apply and
// tracegen. They talk to a server or an agent from anywhere.
package opscli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ijcodelabs/vigil/internal/otlp"
)

// Tracegen sends demo traces of a small shop to an agent's OTLP endpoint
// until ctx ends.
func Tracegen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tracegen", flag.ExitOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:4318", "an agent's OTLP endpoint")
	rate := fs.Float64("rate", 2, "traces a second")
	fs.Parse(args)
	fmt.Fprintf(os.Stderr, "sending demo traces of a small shop to %s, %.3g a second; Ctrl-C stops\n", *endpoint, *rate)
	n, err := otlp.SendDemo(ctx, *endpoint, *rate)
	fmt.Fprintf(os.Stderr, "sent %d traces\n", n)
	return err
}
