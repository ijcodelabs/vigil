// Package opscli holds the operator tools of the vigil command: apply and
// tracegen. They talk to a server or an agent from anywhere.
package opscli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ijcodelabs/vigil/internal/otlp"
)

// Tracegen sends demo traces of a small shop to an agent's OTLP endpoint
// until ctx ends.
func Tracegen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tracegen", flag.ExitOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:4318", "an agent's OTLP endpoint")
	rate := fs.Float64("rate", 2, "traces a second")
	token := fs.String("token", os.Getenv("VIGIL_OTLP_TOKEN"), "token for an agent with otlp.tokens_file (or $VIGIL_OTLP_TOKEN)")
	tokenFile := fs.String("token-file", "", "read the token from a file: the token alone, or an agent's tokens_file (its first token)")
	fs.Parse(args)
	if *tokenFile != "" {
		t, err := firstToken(*tokenFile)
		if err != nil {
			return err
		}
		*token = t
	}
	fmt.Fprintf(os.Stderr, "sending demo traces of a small shop to %s, %.3g a second; Ctrl-C stops\n", *endpoint, *rate)
	n, err := otlp.SendDemo(ctx, *endpoint, *token, *rate)
	fmt.Fprintf(os.Stderr, "sent %d traces\n", n)
	return err
}

// firstToken reads a token from a file holding just the token, or lines of
// "name token" as an agent's otlp.tokens_file has them; # starts a comment.
func firstToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		switch len(f) {
		case 1:
			return f[0], nil
		case 2:
			return f[1], nil
		}
	}
	return "", fmt.Errorf("%s holds no token", path)
}
