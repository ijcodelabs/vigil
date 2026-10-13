package server

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/rules"
)

// The rule file is one document, edited as a whole. These two endpoints
// exist so that Explore can turn the query on screen into an alert with a
// form: the rule is written into the document, which is then checked and
// saved the way the whole file is. Everything else in it stays as it was,
// comments included.

// newAlert is one alert rule as the form describes it.
type newAlert struct {
	Group    string `json:"group"`
	Type     string `json:"type"` // metrics or logs: the kind of group it goes into
	Alert    string `json:"alert"`
	Expr     string `json:"expr"`
	For      string `json:"for"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// handleRuleGroups lists the groups of the rule file, for choosing where a
// new alert goes. Built-in and integration groups are not in the file.
func (s *Server) handleRuleGroups(w http.ResponseWriter, _ *http.Request) {
	type group struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	out := []group{}
	text, err := s.store.Setting(settingRules, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if f, err := rules.Parse(text); err == nil {
		for _, g := range f.Groups {
			out = append(out, group{g.Name, g.Type})
		}
	}
	writeJSON(w, out)
}

func (s *Server) addAlert(w http.ResponseWriter, r *http.Request) {
	var a newAlert
	if !readJSON(w, r, &a) {
		return
	}
	a.Group, a.Alert, a.Expr = strings.TrimSpace(a.Group), strings.TrimSpace(a.Alert), strings.TrimSpace(a.Expr)
	a.For, a.Severity, a.Summary = strings.TrimSpace(a.For), strings.TrimSpace(a.Severity), strings.TrimSpace(a.Summary)
	if a.Type == "" {
		a.Type = rules.TypeMetrics
	}
	switch {
	case a.Type != rules.TypeMetrics && a.Type != rules.TypeLogs:
		http.Error(w, "type must be metrics or logs", http.StatusBadRequest)
		return
	case a.Group == "" || a.Alert == "" || a.Expr == "" || len(a.Group) > 100 || len(a.Alert) > 100:
		http.Error(w, "a group, a name for the alert and an expression are needed", http.StatusBadRequest)
		return
	case a.Group == builtinGroup || strings.Contains(a.Group, "/"):
		// A group called builtin would switch every built-in alert off;
		// names with a slash are those of integrations.
		http.Error(w, fmt.Sprintf("%q cannot be the group: that name is taken by rules that come with vigil", a.Group), http.StatusBadRequest)
		return
	}
	// An alert of a built-in name replaces the built-in one. That is what
	// the rule file is for, but not something to do by accident from a form.
	if b, err := rules.Parse(builtinRules); err == nil {
		for _, g := range b.Groups {
			for _, rule := range g.Rules {
				if rule.Alert == a.Alert {
					http.Error(w, fmt.Sprintf("%s is an alert that comes with vigil. Choose another name, or edit the rule file to replace it.", a.Alert), http.StatusConflict)
					return
				}
			}
		}
	}
	text, err := s.store.Setting(settingRules, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	next, err := editRules(text, a)
	if err != nil {
		http.Error(w, "the rule was not added: "+err.Error(), http.StatusConflict)
		return
	}
	if s.saveRules(w, r, next) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// editRules adds an alert to a group of a rule file, making the group if
// the file has none of that name.
func editRules(text string, a newAlert) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if len(doc.Content) == 0 { // an empty file, or only comments
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", fmt.Errorf("the rule file is not a YAML mapping")
	}
	str := func(v string, style yaml.Style) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: style}
	}
	// list returns the sequence under key, making or emptying it as needed.
	list := func(m *yaml.Node, key string) (*yaml.Node, error) {
		n := mapValue(m, key)
		switch {
		case n == nil:
			n = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			m.Content = append(m.Content, str(key, 0), n)
		case n.Kind == yaml.SequenceNode:
		case len(n.Content) == 0 && (n.Tag == "!!null" || n.Value == ""):
			*n = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		default:
			return nil, fmt.Errorf("%s is not a list", key)
		}
		n.Style = 0 // "groups: []" has to become a block to take an entry
		return n, nil
	}
	groups, err := list(root, "groups")
	if err != nil {
		return "", err
	}
	var group *yaml.Node
	for _, g := range groups.Content {
		if g.Kind != yaml.MappingNode {
			continue
		}
		if n := mapValue(g, "name"); n != nil && n.Value == a.Group {
			group = g
		}
		if rs := mapValue(g, "rules"); rs != nil {
			for _, rule := range rs.Content {
				if n := mapValue(rule, "alert"); rule.Kind == yaml.MappingNode && n != nil && n.Value == a.Alert {
					return "", fmt.Errorf("the rule file already has an alert called %s", a.Alert)
				}
			}
		}
	}
	if group == nil {
		group = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		group.Content = append(group.Content, str("name", 0), str(a.Group, 0))
		if a.Type == rules.TypeLogs {
			group.Content = append(group.Content, str("type", 0), str(rules.TypeLogs, 0))
		}
		groups.Content = append(groups.Content, group)
	} else {
		has := rules.TypeMetrics
		if n := mapValue(group, "type"); n != nil && n.Value != "" {
			has = n.Value
		}
		if has != a.Type {
			return "", fmt.Errorf("group %s holds %s rules, and this is a %s query; choose another group", a.Group, has, a.Type)
		}
	}
	ruleList, err := list(group, "rules")
	if err != nil {
		return "", err
	}
	rule := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	exprStyle := yaml.SingleQuotedStyle // LogsQL and label matchers are full of what YAML reads as its own
	if strings.Contains(a.Expr, "\n") {
		exprStyle = yaml.LiteralStyle
	}
	rule.Content = append(rule.Content, str("alert", 0), str(a.Alert, 0), str("expr", 0), str(a.Expr, exprStyle))
	if a.For != "" {
		rule.Content = append(rule.Content, str("for", 0), str(a.For, 0))
	}
	pair := func(key, k, v string, style yaml.Style) {
		if v == "" {
			return
		}
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		m.Content = append(m.Content, str(k, 0), str(v, style))
		rule.Content = append(rule.Content, str(key, 0), m)
	}
	pair("labels", "severity", a.Severity, 0)
	pair("annotations", "summary", a.Summary, yaml.SingleQuotedStyle) // templates start with a brace
	ruleList.Content = append(ruleList.Content, rule)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return buf.String(), nil
}
