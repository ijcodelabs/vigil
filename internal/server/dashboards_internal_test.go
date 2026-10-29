package server

import (
	"encoding/json"
	"testing"

	"github.com/ijcodelabs/vigil/internal/fleet"
)

// Every dashboard that comes with an integration is one the server would
// accept: they are installed without the checks a saved one gets.
func TestBuiltinIntegrationDashboards(t *testing.T) {
	cat, err := fleet.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cat.Names() {
		if cat[name].Dashboard == nil {
			continue
		}
		body, _ := json.Marshal(cat[name].Dashboard)
		if _, err := parseDashboard(body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
