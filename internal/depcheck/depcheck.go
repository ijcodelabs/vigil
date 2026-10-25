// Package depcheck lets a binary's tests fail when it imports something it
// must not, so the server's dependencies cannot creep back into the agent
// or the relay unnoticed.
package depcheck

import (
	"os/exec"
	"strings"
	"testing"
)

// Server is what only the server may import: its own packages and the
// heavy libraries that come with them (Alertmanager brings an AWS SDK,
// Kafka and more).
var Server = []string{
	"github.com/ijcodelabs/vigil/internal/server",
	"github.com/ijcodelabs/vigil/internal/rules",
	"github.com/ijcodelabs/vigil/internal/alerting",
	"github.com/ijcodelabs/vigil/internal/incidents",
	"github.com/ijcodelabs/vigil/internal/fleet",
	"github.com/ijcodelabs/vigil/internal/cli/servercli",
	"github.com/prometheus/alertmanager",
	"github.com/aws/",
	"github.com/twmb/franz-go",
	"modernc.org/sqlite",
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/yuin/goldmark",
}

// Forbid fails t if the package in the current directory depends, directly
// or not, on any package whose path starts with one of the prefixes.
func Forbid(t *testing.T, prefixes ...string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list is not available here: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, p := range prefixes {
			if matches(dep, p) {
				t.Errorf("imports %s, which it must not", dep)
			}
		}
	}
}

// matches says whether package dep is prefix or lies under it. A prefix
// ending in "/" matches anything that starts with it.
func matches(dep, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(dep, prefix)
	}
	return dep == prefix || strings.HasPrefix(dep, prefix+"/")
}
