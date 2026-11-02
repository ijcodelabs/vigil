package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCollectionSkipping(t *testing.T) {
	// A setting from a newer server: skipped, the rest applied.
	c, skipped, err := ParseCollectionSkipping("interval: 20s\notlp:\n  enabled: true\n  shiny_new: {a: 1}\nfuture_section:\n  x: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(skipped, []string{"otlp.shiny_new", "future_section"}) || c.Interval.Std().Seconds() != 20 || !c.OTLP.Enabled {
		t.Errorf("skipped %v, interval %v, otlp %v", skipped, c.Interval, c.OTLP.Enabled)
	}
	// Nothing unknown: nothing skipped.
	if _, skipped, err := ParseCollectionSkipping("interval: 20s\n"); err != nil || skipped != nil {
		t.Errorf("%v %v", skipped, err)
	}
	// Guarded parts: refused as before.
	for _, doc := range []string{
		"otlp:\n  tls:\n    from_future: x\n",
		"otlp:\n  new_token_rule: x\n",
		"logs:\n  drop_new: x\n",
		"identity_v2: {}\n",
		"new_block:\n  inner:\n    drop_attributes: {actor_ref: true}\n", // a privacy setting inside a skipped block
	} {
		if _, _, err := ParseCollectionSkipping(doc); err == nil || !strings.Contains(err.Error(), "upgrade the agent") {
			t.Errorf("%q: %v", doc, err)
		}
	}
	// Other mistakes are still refused.
	if _, _, err := ParseCollectionSkipping("interval: [1]\n"); err == nil {
		t.Error("a wrong type was accepted")
	}
}
