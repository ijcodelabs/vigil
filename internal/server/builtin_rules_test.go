package server

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/rules"
)

func alertNames(g rules.Group) map[string]bool {
	out := map[string]bool{}
	for _, r := range g.Rules {
		out[r.Alert] = true
	}
	return out
}

// The rule file kept in git has the last word: an alert it defines replaces
// the built-in one of that name, and a group named "builtin" replaces them all.
func TestBuiltinRulesYieldToTheRuleFile(t *testing.T) {
	parse := func(text string) *rules.File {
		t.Helper()
		f, err := rules.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	g, ok := builtinRuleGroup(parse("groups: []\n"))
	names := alertNames(g)
	if !ok || g.Name != builtinGroup || !names["HostDown"] || !names["ProbeFailing"] || !names["CertificateExpiresSoon"] || !names["CertificateExpired"] {
		t.Fatalf("with an empty rule file: ok=%v group=%q alerts=%v", ok, g.Name, names)
	}
	all := len(g.Rules)

	g, ok = builtinRuleGroup(parse(`
groups:
  - name: mine
    rules:
      - alert: HostDown
        expr: vigil_agent_up == 0
        for: 10m
`))
	if names = alertNames(g); !ok || names["HostDown"] || len(g.Rules) != all-1 {
		t.Errorf("an own HostDown should replace the built-in one only: ok=%v alerts=%v", ok, names)
	}

	if _, ok = builtinRuleGroup(parse("groups:\n  - name: builtin\n    rules: []\n")); ok {
		t.Error("a group named builtin in the rule file should switch the built-in rules off")
	}
}
