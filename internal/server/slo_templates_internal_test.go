package server

import (
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/fleet"
	"github.com/ijcodelabs/vigil/internal/slo"
)

// Every template that comes with vigil, its own and the integrations',
// makes an SLO that parses, for a plain value and for an awkward one.
func TestSLOTemplatesParse(t *testing.T) {
	catalog, err := fleet.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	list := append([]fleet.SLOTemplate{}, builtinSLOTemplates...)
	for _, in := range catalog {
		list = append(list, in.SLOs...)
	}
	if len(list) < 4 {
		t.Fatalf("only %d templates", len(list))
	}
	for _, tpl := range list {
		for _, v := range []string{"shop-api", "be_App.v2:8080/x@y"} {
			name, doc := tpl.Fill(v)
			def, err := slo.Parse(name, doc)
			if err != nil {
				t.Errorf("%s with %q: %v\n%s", tpl.ID, v, err, doc)
				continue
			}
			if good, _ := def.Queries(); !strings.Contains(good+def.Source(), v) {
				t.Errorf("%s: the value is not in the SLO: %s", tpl.ID, good)
			}
		}
	}
	if name, _ := builtinSLOTemplates[0].Fill("Shop_Home"); name != "check-shop-home" {
		t.Errorf("name: %s", name)
	}
}
