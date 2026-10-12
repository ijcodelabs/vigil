package server

import (
	"bytes"
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
	name := "agent-" + strings.ToLower(id)
	if !profileNameRe.MatchString(name) {
		http.Error(w, "this agent's name cannot be a profile name; set docker.container_logs in a profile that selects it", http.StatusBadRequest)
		return
	}
	f := &s.fleet
	f.mu.Lock()
	_, known := f.attrs[id]
	row, exists := f.rows[name]
	f.mu.Unlock()
	if !known {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	text := fmt.Sprintf("# Settings for the agent %s alone.\nselect: ['agent=%q']\npriority: 1000\n", id, id)
	if exists {
		text = row.Doc
	}
	text, err := editContainerLogs(text, strings.TrimSpace(req.Container), req.Collect)
	if err != nil {
		http.Error(w, "profile "+name+" cannot be changed here: "+err.Error(), http.StatusBadRequest)
		return
	}
	save := r.Clone(r.Context())
	save.SetPathValue("name", name)
	save.Body = io.NopCloser(strings.NewReader(text))
	s.putProfile(w, save)
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
