package server

import (
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/fleet"
)

// Switching a container's logs off and on edits one entry of the profile
// and leaves the rest of the document, comments included, as it was.
func TestEditContainerLogs(t *testing.T) {
	start := "# Settings for the agent web-1 alone.\nselect: ['agent=\"web-1\"']\npriority: 1000\n"
	off, err := editContainerLogs(start, "haproxy", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(off, "# Settings for the agent web-1 alone.") || !strings.Contains(off, "container_logs:\n      \"haproxy\": false") {
		t.Errorf("after switching haproxy off:\n%s", off)
	}
	p, err := fleet.ParseProfile("agent-web-1", off)
	if err != nil {
		t.Fatalf("the result is not a valid profile: %v\n%s", err, off)
	}
	if logs := p.Config["docker"].(map[string]any)["container_logs"].(map[string]any); logs["haproxy"] != false {
		t.Errorf("parsed: %v", logs)
	}

	both, _ := editContainerLogs(off, "node-*", false)
	if !strings.Contains(both, `"haproxy": false`) || !strings.Contains(both, `"node-*": false`) {
		t.Errorf("a second container:\n%s", both)
	}
	// Back on: the entry goes, and with the last one the empty sections.
	one, _ := editContainerLogs(both, "haproxy", true)
	if strings.Contains(one, "haproxy") || !strings.Contains(one, `"node-*": false`) {
		t.Errorf("haproxy back on:\n%s", one)
	}
	none, _ := editContainerLogs(one, "node-*", true)
	if strings.Contains(none, "config") || !strings.Contains(none, "priority: 1000") {
		t.Errorf("all back on should leave no empty config:\n%s", none)
	}
	// Nothing switched it off here, so something wider did: say true.
	if on, _ := editContainerLogs(start, "shop-api", true); !strings.Contains(on, `"shop-api": true`) {
		t.Errorf("overruling a wider pattern:\n%s", on)
	}
	// Other settings in the profile stay.
	rich := "select: ['agent=\"web-1\"']\npriority: 1000\nconfig:\n  interval: 30s   # slow host\n  docker:\n    template: quiet\n"
	if out, _ := editContainerLogs(rich, "haproxy", false); !strings.Contains(out, "interval: 30s # slow host") && !strings.Contains(out, "interval: 30s   # slow host") ||
		!strings.Contains(out, "template: quiet") || !strings.Contains(out, `"haproxy": false`) {
		t.Errorf("a profile with other settings:\n%s", out)
	}
}
