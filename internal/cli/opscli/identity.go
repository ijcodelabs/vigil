package opscli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ijcodelabs/vigil/internal/pki"
)

const identityUsage = `vigil identity: names that may hold a certificate from the server's authority

  vigil identity list
  vigil identity approve NAME -agents REGEXP                    a client (a program that sends)
  vigil identity approve NAME -agents REGEXP -endpoint -names 192.0.2.10,otlp.internal
  vigil identity revoke NAME     every certificate for it is refused at its next request
  vigil identity delete NAME

approve on a revoked name approves it again, at a new epoch. Connection:
-server, -token, -ca-pin, -ca-file (or VIGIL_SERVER, VIGIL_ADMIN_TOKEN, VIGIL_CA_PIN).`

// Identity manages identities (memory/context/identity.md) through the
// admin API, so nobody needs curl for it.
func Identity(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(os.Stderr, identityUsage)
		return nil
	}
	cmd, args := args[0], args[1:]
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") { // the name may come before the flags
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("identity "+cmd, flag.ExitOnError)
	server := fs.String("server", os.Getenv("VIGIL_SERVER"), "server address (or $VIGIL_SERVER)")
	token := fs.String("token", os.Getenv("VIGIL_ADMIN_TOKEN"), "admin token (or $VIGIL_ADMIN_TOKEN)")
	caPin := fs.String("ca-pin", os.Getenv("VIGIL_CA_PIN"), "fingerprint of the server's certificate authority (or $VIGIL_CA_PIN)")
	caFile := fs.String("ca-file", "", "certificate file to trust instead of the system's authorities")
	agents := fs.String("agents", "", "approve: the agents that may hold it, as a regular expression of agent ids")
	endpoint := fs.Bool("endpoint", false, "approve: an OTLP endpoint's serving certificate instead of a client's")
	names := fs.String("names", "", "approve -endpoint: its host names and addresses, comma-separated")
	fs.Parse(args)
	if name == "" {
		name = fs.Arg(0)
	}
	if *server == "" || *token == "" {
		return errors.New("identity needs -server and -token (or VIGIL_SERVER and VIGIL_ADMIN_TOKEN)")
	}
	hc, err := pki.HTTPClient(pki.ClientOptions{ServerURL: *server, CAFile: *caFile, CAPin: *caPin})
	if err != nil {
		return err
	}
	base := strings.TrimRight(*server, "/") + "/api/v1/identities"
	call := func(method, u, body string) (string, error) {
		req, err := http.NewRequest(method, u, strings.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode/100 != 2 {
			return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return string(b), nil
	}
	one := func() string { return base + "/" + url.PathEscape(name) }
	if cmd != "list" && name == "" {
		return fmt.Errorf("identity %s needs a NAME\n\n%s", cmd, identityUsage)
	}

	switch cmd {
	case "list":
		body, err := call(http.MethodGet, base, "")
		if err != nil {
			return err
		}
		return printIdentities(os.Stdout, body)
	case "approve":
		if *agents == "" {
			return errors.New("identity approve needs -agents (a regular expression of the agent ids that may hold it)")
		}
		in := map[string]any{"kind": "client", "agents": *agents}
		if *endpoint {
			var list []string
			for _, n := range strings.Split(*names, ",") {
				if n = strings.TrimSpace(n); n != "" {
					list = append(list, n)
				}
			}
			if len(list) == 0 {
				return errors.New("identity approve -endpoint needs -names (its host names and addresses)")
			}
			in["kind"], in["names"] = "endpoint", list
		} else if *names != "" {
			return errors.New("-names is for -endpoint only")
		}
		b, _ := json.Marshal(in)
		body, err := call(http.MethodPut, one(), string(b))
		if err != nil {
			return err
		}
		return printIdentities(os.Stdout, "["+body+"]")
	case "revoke":
		body, err := call(http.MethodPost, one()+"/revoke", "")
		if err != nil {
			return err
		}
		if err := printIdentities(os.Stdout, "["+body+"]"); err != nil {
			return err
		}
		fmt.Println("Every certificate issued for it is refused at its next request (endpoints learn it within a minute). `vigil identity approve` it again for a new epoch.")
		return nil
	case "delete":
		_, err := call(http.MethodDelete, one(), "")
		if err == nil {
			fmt.Println("deleted", name)
		}
		return err
	}
	return fmt.Errorf("identity: unknown command %q\n\n%s", cmd, identityUsage)
}

func printIdentities(w io.Writer, body string) error {
	var list []struct {
		Name       string     `json:"name"`
		Kind       string     `json:"kind"`
		Agents     string     `json:"agents"`
		Names      []string   `json:"names"`
		Epoch      int        `json:"epoch"`
		Revoked    bool       `json:"revoked"`
		LastIssued *time.Time `json:"last_issued"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		return fmt.Errorf("unexpected answer: %w", err)
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tAGENTS\tEPOCH\tSTATE\tLAST ISSUED\tNAMES")
	for _, id := range list {
		state, issued := "approved", "never"
		if id.Revoked {
			state = "revoked"
		}
		if id.LastIssued != nil {
			issued = id.LastIssued.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", id.Name, id.Kind, id.Agents, id.Epoch, state, issued, strings.Join(id.Names, ","))
	}
	return tw.Flush()
}
