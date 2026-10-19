package fleet

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
)

// Redaction rules merge by name and dropped labels by key, so a profile
// for one tool adds to what a base profile set instead of replacing it.
func TestResolveRedactAndDropLabels(t *testing.T) {
	base := profile(t, "base", `
select: ['agent="a1"']
priority: 1
config:
  otlp: {enabled: true, metrics: {drop_labels: {session_id: true, user_id: true}}}
  redact:
    rules:
      - {name: card, match: '\d{16}'}
      - {name: customer, match: 'customer=\S+'}
`)
	tool := profile(t, "tool", `
select: ['agent="a1"']
priority: 2
config:
  otlp: {metrics: {drop_labels: {user_id: false, organization_id: true}}}
  redact:
    rules:
      - {name: customer, enabled: false}
      - {name: ticket, match: 'TICKET-\d+'}
`)
	r, err := Resolve([]*Profile{base, tool}, nil, Attributes("a1", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseCollection(r.YAML)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.YAML)
	}
	if d := c.OTLP.Metrics.DropLabels; !d["session_id"] || d["user_id"] || !d["organization_id"] {
		t.Errorf("drop_labels: %v", d)
	}
	var names []string
	for _, ru := range c.Redact.Rules {
		names = append(names, ru.Name)
	}
	if len(names) != 2 || names[0] != "card" || names[1] != "ticket" {
		t.Errorf("rules: %v", names)
	}
}

func TestRedactRuleChecked(t *testing.T) {
	for _, text := range []string{
		"redact: {rules: [{name: bad, match: '('}]}",
		"redact: {rules: [{match: 'x'}]}",
		"redact: {rules: [{name: a, match: 'x'}, {name: a, match: 'y'}]}",
		"redact: {rules: [{name: empty}]}",
	} {
		if c, err := config.ParseCollection(text); err == nil && c.Check() == nil {
			t.Errorf("accepted: %s", text)
		}
	}
	c, err := config.ParseCollection("redact: {builtin: false, rules: [{name: off, enabled: false}]}")
	if err == nil {
		err = c.Check()
	}
	if err != nil {
		t.Errorf("a switched-off rule needs no pattern: %v", err)
	}
}
