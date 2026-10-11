// Package fleet resolves what each agent should collect. A profile is a
// fragment of agent config with a selector; the config of an agent is the
// merge of every profile whose selector matches it, lowest priority first.
package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	amlabels "github.com/prometheus/alertmanager/pkg/labels"
	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Profile is one YAML document:
//
//	select: ['env="prod"', 'role=~"db|cache"']   # all must match; empty selects every agent
//	                                             # (=, !=, =~, !~ as in Prometheus: a regular
//	                                             # expression must match the whole value)
//	priority: 20                                 # higher is merged later and wins
//	config:                                      # a fragment of agent config
//	  scrape: {...}
type Profile struct {
	Name     string   `yaml:"name,omitempty" json:"name"`
	Select   []string `yaml:"select" json:"select"`
	Priority int      `yaml:"priority" json:"priority"`
	// Integrations switched on for the selected agents, with parameters.
	// Their config is merged before this profile's own, which can adjust it.
	Integrations IntegrationRefs `yaml:"integrations" json:"-"`
	Config       map[string]any  `yaml:"config" json:"-"`
	// Rollout, when set, makes every change to this profile reach its agents
	// in steps instead of all at once.
	Rollout *Rollout `yaml:"rollout" json:"rollout,omitempty"`

	matchers []*amlabels.Matcher
}

// IntegrationRefs maps an integration name to the parameters given for it.
// In YAML it is either a list of names or a map of name to parameters.
type IntegrationRefs map[string]map[string]string

func (r *IntegrationRefs) UnmarshalYAML(n *yaml.Node) error {
	out := IntegrationRefs{}
	switch n.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			out[name] = nil
		}
	default:
		var m map[string]map[string]string
		if err := n.Decode(&m); err != nil {
			return fmt.Errorf("line %d: integrations is a list of names, or a map of name to parameters", n.Line)
		}
		for name, params := range m {
			out[name] = params
		}
	}
	*r = out
	return nil
}

// ParseProfile reads and checks one profile document. The config fragment
// must use known fields, so a typo is caught here; whether the merged
// result makes sense is checked by Resolve.
func ParseProfile(name, text string) (*Profile, error) {
	if !nameRe.MatchString(name) {
		return nil, errors.New("profile names are lower-case letters, digits and . _ -")
	}
	p := &Profile{}
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if p.Name != "" && p.Name != name {
		return nil, fmt.Errorf("the document says name: %s, but it is being saved as %s", p.Name, name)
	}
	p.Name = name
	for _, sel := range p.Select {
		m, err := amlabels.ParseMatcher(sel)
		if err != nil {
			return nil, fmt.Errorf("select %q: %v (write it like env=\"prod\" or role=~\"db|cache\")", sel, err)
		}
		p.matchers = append(p.matchers, m)
	}
	if err := p.Rollout.check(); err != nil {
		return nil, fmt.Errorf("rollout: %w", err)
	}
	if p.Config == nil {
		p.Config = map[string]any{}
	}
	// Decode the fragment as a collection only to reject unknown fields.
	frag, _ := yaml.Marshal(p.Config)
	fdec := yaml.NewDecoder(bytes.NewReader(frag))
	fdec.KnownFields(true)
	if err := fdec.Decode(&config.Collection{}); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	return p, nil
}

// Matches reports whether the profile applies to an agent with these
// attributes. A label the agent does not have counts as empty.
func (p *Profile) Matches(attrs map[string]string) bool {
	for _, m := range p.matchers {
		if !m.Matches(attrs[m.Name]) {
			return false
		}
	}
	return true
}

// Matching returns the profiles that apply, in the order they are merged.
func Matching(profiles []*Profile, attrs map[string]string) []*Profile {
	var out []*Profile
	for _, p := range profiles {
		if p.Matches(attrs) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Attributes are what selectors are matched against: the labels from the
// agent's file, overlaid with the facts it reports and its id. Facts win,
// so a label cannot pretend to be one.
func Attributes(agentID string, labels, facts map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		out[k] = v
	}
	for k, v := range facts {
		out[k] = v
	}
	out["agent"] = agentID
	return out
}
