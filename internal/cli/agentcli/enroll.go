package agentcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/pki"
)

// labelFlags collects repeated -label key=value.
type labelFlags map[string]string

func (l labelFlags) String() string { return "" }
func (l labelFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("labels are written key=value")
	}
	l[k] = val
	return nil
}

// Enroll exchanges an enrolment token for this host's own token and
// writes the agent's config file with it.
func Enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", os.Getenv("VIGIL_SERVER"), "server address (or $VIGIL_SERVER)")
	token := fs.String("token", os.Getenv("VIGIL_ENROLL_TOKEN"), "enrolment token (or $VIGIL_ENROLL_TOKEN)")
	caPin := fs.String("ca-pin", "", "fingerprint of the server's certificate authority (sha256:...)")
	caFile := fs.String("ca-file", "", "certificate file to trust instead of the system's authorities")
	id := fs.String("id", "", "agent id (default: the host name)")
	out := fs.String("config", "/etc/vigil/agent.yaml", "agent config file to write")
	labels := labelFlags{}
	fs.Var(labels, "label", "label for this host, key=value; repeat for more")
	fs.Parse(args)
	if *server == "" || *token == "" {
		return errors.New("enroll needs -server and -token (or VIGIL_SERVER and VIGIL_ENROLL_TOKEN)")
	}
	if *id == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("cannot read the host name, give -id: %w", err)
		}
		*id = h
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; this host looks enrolled. Remove the file first to enrol again", *out)
	}
	// Check the file can be written before spending a use of the token.
	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		f.Close()
		if !done {
			os.Remove(*out)
		}
	}()

	hc, err := pki.HTTPClient(pki.ClientOptions{ServerURL: *server, CAFile: *caFile, CAPin: *caPin})
	if err != nil {
		return err
	}
	agentToken, _, err := agent.Enroll(ctx, hc, *server, *token, *id)
	if err != nil {
		return err
	}

	srv := map[string]string{"url": strings.TrimRight(*server, "/"), "token": agentToken}
	if *caPin != "" {
		srv["ca_pin"] = *caPin
	}
	if *caFile != "" {
		if abs, err := filepath.Abs(*caFile); err == nil {
			*caFile = abs
		}
		srv["ca_file"] = *caFile
	}
	var b strings.Builder
	b.WriteString("# Written by `vigil-agent enroll`. The token belongs to this agent id only.\nserver:\n")
	for _, k := range []string{"url", "token", "ca_pin", "ca_file"} {
		if v, ok := srv[k]; ok {
			q, _ := yaml.Marshal(v)
			fmt.Fprintf(&b, "  %s: %s", k, q)
		}
	}
	q, _ := yaml.Marshal(*id)
	fmt.Fprintf(&b, "agent_id: %s", q)
	b.WriteString("# Labels say what this host is. Profiles on the server select agents by them.\n")
	if len(labels) == 0 {
		b.WriteString("labels: {}\n")
	} else {
		b.WriteString("labels:\n")
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			kq, _ := yaml.Marshal(k)
			vq, _ := yaml.Marshal(labels[k])
			fmt.Fprintf(&b, "  %s: %s", strings.TrimSpace(string(kq)), vq)
		}
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return fmt.Errorf("enrolled as %s, but writing %s failed: %w. Revoke the agent's token on the Fleet page before trying again", *id, *out, err)
	}
	done = true
	fmt.Printf("enrolled as %s; wrote %s\nstart the agent with: vigil-agent -config %s\n", *id, *out, *out)
	return nil
}
