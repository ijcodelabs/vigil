package agentcli

import (
	"context"
	"flag"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/ijcodelabs/vigil/internal/claudehook"
)

// ClaudeHook is a Claude Code command hook: it reads the hook's input
// from stdin and sends it to the local agent as one log line. It prints
// nothing and returns nil whatever happens, because Claude Code gives a
// hook's output to the model and blocks a prompt on exit code 2; an audit
// line that cannot be sent is lost rather than getting in the way.
func ClaudeHook(args []string) error {
	fs := flag.NewFlagSet("claude-hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	endpoint := fs.String("endpoint", envOr("VIGIL_OTLP_ENDPOINT", "http://127.0.0.1:4318"), "the agent's OTLP endpoint")
	content := fs.Bool("content", false, "also send the text of prompts and of subagents' instructions")
	timeout := fs.Duration("timeout", 500*time.Millisecond, "give up on the agent after this long")
	if fs.Parse(args) != nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		return nil
	}
	o := claudehook.Options{Content: *content}
	if u, err := user.Current(); err == nil {
		o.User = u.Username
	}
	req, err := claudehook.Record(raw, time.Now(), o)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	claudehook.Send(ctx, *endpoint, req)
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
