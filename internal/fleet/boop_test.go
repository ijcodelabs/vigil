package fleet

import (
	"bufio"
	"encoding/json"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/logs"
)

// The tenant CA of the contract's test data.
const tenantCA = "SHA256:38lMZN9s593n4PAe0oPt/XESnoQduAWmRgcoOOzgK00"

// The Boop integrations use only metric names from Boop's contract, so a
// rename on Boop's side shows up here and not as an empty panel.
func TestBoopContractMetrics(t *testing.T) {
	b, err := os.ReadFile("testdata/boop/contract-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "#") {
			for _, n := range strings.Fields(line) {
				known[n] = true
			}
		}
	}
	name := regexp.MustCompile(`\b(?:aps|boop)_[a-z0-9_]+`)
	cat, _ := Builtin()
	for _, in := range []string{"boop", "boop_jump_gateway", "boop_jump_agent"} {
		text, _ := json.Marshal([]any{cat[in].Rules, cat[in].Dashboard, cat[in].Queries})
		for _, n := range name.FindAllString(string(text), -1) {
			base := n
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if s, ok := strings.CutSuffix(n, suffix); ok && known[s] {
					base = s
				}
			}
			if !known[base] {
				t.Errorf("%s uses %s, which is not in the contract", in, n)
			}
		}
	}
	// The fixtures, too, have only contract names.
	for _, f := range []string{"metrics-boop.txt", "metrics-export.txt", "metrics-gateway.txt", "metrics-agent.txt"} {
		b, _ := os.ReadFile("testdata/boop/" + f)
		for _, n := range name.FindAllString(string(b), -1) {
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum"), "_count")
			if !known[n] && !known[base] {
				t.Errorf("%s has %s, which the list does not", f, n)
			}
		}
	}
}

type journalLine struct {
	Unit       string `json:"_SYSTEMD_UNIT"`
	Identifier string `json:"SYSLOG_IDENTIFIER"`
	Message    string `json:"MESSAGE"`
	Expect     string `json:"TEST_EXPECT"`
}

// The sshd template takes the contract's real sshd lines apart, and the
// shadow-access rule's conditions, applied to its fields, classify each
// line the way the contract says. The conditions are read out of the
// rule's own expression, so the test follows the rule.
func TestBoopShadowAccess(t *testing.T) {
	cat, _ := Builtin()
	r, err := Resolve([]*Profile{profile(t, "p", "integrations: [sshd]")}, cat, Attributes("x", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseCollection(r.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.Validate(c); err != nil {
		t.Fatal(err)
	}
	tm, err := logs.Compile("sshd_auth", c.Logs.Templates["sshd_auth"])
	if err != nil {
		t.Fatal(err)
	}

	groups, err := cat["boop"].RuleGroups(map[string]string{"tenant_ca": tenantCA, "guarded_agents": "web-prod-.*"})
	if err != nil {
		t.Fatal(err)
	}
	var expr string
	for _, g := range groups {
		for _, rule := range g.Rules {
			if rule.Alert == "SshLoginNotThroughBoopJump" {
				expr = rule.Expr
			}
		}
	}
	part := func(re string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(expr)
		if m == nil {
			t.Fatalf("%s not in %s", re, expr)
		}
		return m[1]
	}
	unitRe := regexp.MustCompile(part(`unit:~"([^"]+)"`))
	msgRe := regexp.MustCompile(part(`_msg:~"([^"]+)"`))
	typeRe := regexp.MustCompile(part(`ssh_key_type:~"([^"]+)"`))
	keyIDRe := regexp.MustCompile(part(`ssh_key_id:~"([^"]+)"`))
	if ca := part(`ssh_ca_fp:="([^"]+)"`); ca != tenantCA {
		t.Fatalf("CA in the rule: %s", ca)
	}

	f, err := os.Open("testdata/boop/sshd-journal.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var l journalLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		// The journald source picks the template by unit or identifier.
		templated := false
		for pattern, name := range c.Logs.Journald.UnitTemplates {
			for _, s := range []string{l.Unit, l.Identifier} {
				if ok, _ := path.Match(pattern, s); ok && name == "sshd_auth" {
					templated = true
				}
			}
		}
		fields := map[string]string{}
		if templated {
			_, _, _, fields, _ = tm.Process(l.Message)
		}
		got := "ignore"
		if unitRe.MatchString(l.Unit) && msgRe.MatchString(l.Message) {
			jump := typeRe.MatchString(fields["ssh_key_type"]) && keyIDRe.MatchString(fields["ssh_key_id"]) && fields["ssh_ca_fp"] == tenantCA
			got = map[bool]string{true: "jump", false: "shadow"}[jump]
		}
		want, _, _ := strings.Cut(l.Expect, ":")
		if l.Expect == "shadow:serial_mismatch" {
			want = "jump" // the serial cannot be recomputed in a log rule
		}
		if got != want {
			t.Errorf("%s (%s): got %s, fields %v\n  %s", l.Expect, l.Unit, got, fields, l.Message)
		}
		// Whatever the Key ID says, the CA is the last field of the line.
		if strings.Contains(l.Message, " CA ") && templated {
			last := l.Message[strings.LastIndex(l.Message, " ")+1:]
			if fields["ssh_ca_fp"] != last {
				t.Errorf("%s: CA %q, the line ends with %q", l.Expect, fields["ssh_ca_fp"], last)
			}
		}
	}
	if n < 8 {
		t.Errorf("only %d lines in the test data", n)
	}
}
