package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Whose container logs an agent collects is a setting like any other
// (docker.container_logs in a profile). These two endpoints exist so that
// the Log volume page can switch one container off with a click: the change
// is written into the profile of that one agent, agent-<id>, and saved the
// way any profile is, with the same checks and the same history.

var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (s *Server) containerLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/fleet/container-logs", s.viewer(s.handleContainerLogs))
	mux.HandleFunc("POST /api/v1/agents/{id}/container-logs", s.admin(s.setContainerLogs))
	mux.HandleFunc("GET /api/v1/fleet/log-drops", s.viewer(s.handleLogDrops))
	mux.HandleFunc("POST /api/v1/agents/{id}/log-drops", s.admin(s.addLogDrop))
	mux.HandleFunc("DELETE /api/v1/agents/{id}/log-drops/{name}", s.admin(s.removeLogDrop))
}

// logDrop is one logs.drop rule as the Log volume and Logs pages deal in it.
type logDrop struct {
	Profile   string   `json:"profile,omitempty"`
	Select    []string `json:"select,omitempty"`
	Name      string   `json:"name"`
	Container string   `json:"container,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Pod       string   `json:"pod,omitempty"`
	App       string   `json:"app,omitempty"`
	Match     string   `json:"match"`
}

// handleLogDrops lists the logs.drop rules of every profile.
func (s *Server) handleLogDrops(w http.ResponseWriter, _ *http.Request) {
	out := []logDrop{}
	f := &s.fleet
	f.mu.Lock()
	for _, p := range f.profiles {
		logs, _ := p.Config["logs"].(map[string]any)
		items, _ := logs["drop"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if on, isBool := m["enabled"].(bool); isBool && !on {
				continue
			}
			str := func(k string) string { v, _ := m[k].(string); return v }
			out = append(out, logDrop{Profile: p.Name, Select: p.Select, Name: str("name"), Container: str("container"),
				Unit: str("unit"), Pod: str("pod"), App: str("app"), Match: str("match")})
		}
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, out)
}

// agentProfile returns the name and current text of the profile that holds
// one agent's own settings, with the text it starts from if there is none.
func (s *Server) agentProfile(w http.ResponseWriter, id string) (name, text string, ok bool) {
	name = "agent-" + strings.ToLower(id)
	if !profileNameRe.MatchString(name) {
		http.Error(w, "this agent's name cannot be a profile name; make the change in a profile that selects it", http.StatusBadRequest)
		return "", "", false
	}
	f := &s.fleet
	f.mu.Lock()
	_, known := f.attrs[id]
	row, exists := f.rows[name]
	f.mu.Unlock()
	if !known {
		http.Error(w, "no such agent", http.StatusNotFound)
		return "", "", false
	}
	if exists {
		return name, row.Doc, true
	}
	return name, fmt.Sprintf("# Settings for the agent %s alone.\nselect: ['agent=%q']\npriority: 1000\n", id, id), true
}

// saveAgentProfile saves the text the way any profile is saved: checked
// against every agent first, with a new version in its history.
func (s *Server) saveAgentProfile(w http.ResponseWriter, r *http.Request, name, text string) {
	save := r.Clone(r.Context())
	save.SetPathValue("name", name)
	save.Body = io.NopCloser(strings.NewReader(text))
	s.putProfile(w, save)
}

var dropNameJunk = regexp.MustCompile(`[^a-z0-9]+`)

// addLogDrop adds a rule to one agent: lines of a source (a container, a
// systemd unit, a pod or an app) whose message matches are no longer sent.
// An empty match means every line of that source.
func (s *Server) addLogDrop(w http.ResponseWriter, r *http.Request) {
	var d logDrop
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&d); err != nil {
		http.Error(w, "the body is not JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	source := d.Container + d.Unit + d.Pod + d.App
	if source == "" {
		http.Error(w, "say whose lines: container, unit, pod or app", http.StatusBadRequest)
		return
	}
	if _, err := regexp.Compile(d.Match); err != nil {
		http.Error(w, "match is not a regular expression: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, text, ok := s.agentProfile(w, r.PathValue("id"))
	if !ok {
		return
	}
	// A name people can read, made unique by what the rule matches.
	sum := sha256.Sum256([]byte(source + "\x00" + d.Match))
	slug := strings.Trim(dropNameJunk.ReplaceAllString(strings.ToLower(source), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	d.Name = strings.Trim(slug+"-"+hex.EncodeToString(sum[:3]), "-")
	text, err := editLogDrops(text, &d, "")
	if err != nil {
		http.Error(w, "profile "+name+" cannot be changed here: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.saveAgentProfile(w, r, name, text)
}

func (s *Server) removeLogDrop(w http.ResponseWriter, r *http.Request) {
	name, text, ok := s.agentProfile(w, r.PathValue("id"))
	if !ok {
		return
	}
	text, err := editLogDrops(text, nil, r.PathValue("name"))
	if err != nil {
		http.Error(w, "profile "+name+" cannot be changed here: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.saveAgentProfile(w, r, name, text)
}

// editLogDrops adds a rule to config.logs.drop of a profile document, or
// removes the one called remove, keeping everything else in the document.
func editLogDrops(text string, add *logDrop, remove string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("it is not a YAML mapping")
	}
	m := doc.Content[0]
	chain, keys := []*yaml.Node{m}, []string{"config", "logs", "drop"}
	for i, key := range keys {
		kind, tag := yaml.MappingNode, "!!map"
		if i == len(keys)-1 {
			kind, tag = yaml.SequenceNode, "!!seq"
		}
		next := mapValue(m, key)
		if next == nil {
			next = &yaml.Node{Kind: kind, Tag: tag}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		} else if next.Kind != kind {
			if len(next.Content) > 0 || (next.Tag != "!!null" && next.Value != "") {
				return "", fmt.Errorf("%s is not what a profile has there", key)
			}
			*next = yaml.Node{Kind: kind, Tag: tag}
		}
		next.Style = 0
		m = next
		chain = append(chain, m)
	}
	list := m
	name := remove
	if add != nil {
		name = add.Name
	}
	kept := list.Content[:0]
	for _, item := range list.Content { // a rule of that name is replaced
		if n := mapValue(item, "name"); item.Kind != yaml.MappingNode || n == nil || n.Value != name {
			kept = append(kept, item)
		}
	}
	list.Content = kept
	if add != nil {
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		put := func(k, v string, style yaml.Style) {
			item.Content = append(item.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: style})
		}
		put("name", add.Name, 0)
		for k, v := range map[string]string{"container": add.Container, "unit": add.Unit, "pod": add.Pod, "app": add.App} {
			if v != "" {
				put(k, v, yaml.DoubleQuotedStyle)
			}
		}
		if add.Match != "" {
			put("match", add.Match, yaml.SingleQuotedStyle)
		}
		list.Content = append(list.Content, item)
	}
	for i := len(chain) - 1; i > 0 && len(chain[i].Content) == 0; i-- {
		parent := chain[i-1]
		for j := 0; j+1 < len(parent.Content); j += 2 {
			if parent.Content[j].Value == keys[i-1] {
				parent.Content = append(parent.Content[:j], parent.Content[j+2:]...)
				break
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// handleContainerLogs lists every container_logs entry of every profile:
// what was switched off (or back on), by which profile, for which agents.
func (s *Server) handleContainerLogs(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Profile   string   `json:"profile"`
		Select    []string `json:"select"`
		Container string   `json:"container"`
		Collect   bool     `json:"collect"`
	}
	out := []entry{}
	f := &s.fleet
	f.mu.Lock()
	for _, p := range f.profiles {
		docker, _ := p.Config["docker"].(map[string]any)
		logs, _ := docker["container_logs"].(map[string]any)
		for pattern, v := range logs {
			on, _ := v.(bool)
			out = append(out, entry{Profile: p.Name, Select: p.Select, Container: pattern, Collect: on})
		}
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].Container < out[j].Container
	})
	writeJSON(w, out)
}

// setContainerLogs switches the logs of one container (a name, or a pattern
// with * and ?) off or back on for one agent.
func (s *Server) setContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Container string `json:"container"`
		Collect   bool   `json:"collect"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Container) == "" || len(req.Container) > 200 {
		http.Error(w, `the body must be {"container": "name or pattern", "collect": true or false}`, http.StatusBadRequest)
		return
	}
	name, text, ok := s.agentProfile(w, id)
	if !ok {
		return
	}
	text, err := editContainerLogs(text, strings.TrimSpace(req.Container), req.Collect)
	if err != nil {
		http.Error(w, "profile "+name+" cannot be changed here: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.saveAgentProfile(w, r, name, text)
}

// editContainerLogs sets config.docker.container_logs[container] in a
// profile document, keeping everything else in it, comments included.
// Switching off writes false. Switching on removes a false written before;
// where there is none, something wider switched the container off, and
// true is written to overrule it.
func editContainerLogs(text, container string, collect bool) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("it is not a YAML mapping")
	}
	m := doc.Content[0]
	path := []*yaml.Node{m}
	for _, key := range []string{"config", "docker", "container_logs"} {
		next := mapValue(m, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		} else if next.Kind != yaml.MappingNode {
			if len(next.Content) > 0 || (next.Tag != "!!null" && next.Value != "") {
				return "", fmt.Errorf("%s is not a mapping", key)
			}
			*next = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		next.Style = 0 // block style, so the entry gets a line of its own
		m = next
		path = append(path, m)
	}
	had := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == container {
			had = m.Content[i+1].Value == "false"
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			break
		}
	}
	if !collect || !had {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: container, Style: yaml.DoubleQuotedStyle},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(collect)})
	}
	// Leave no empty sections behind when the last entry goes.
	keys := []string{"config", "docker", "container_logs"}
	for i := len(path) - 1; i > 0 && len(path[i].Content) == 0; i-- {
		parent := path[i-1]
		for j := 0; j+1 < len(parent.Content); j += 2 {
			if parent.Content[j].Value == keys[i-1] {
				parent.Content = append(parent.Content[:j], parent.Content[j+2:]...)
				break
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
