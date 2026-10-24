package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/facts"
	"github.com/ijcodelabs/vigil/internal/fleet"
)

const (
	settingProfilesMigrated = "profiles_migrated"
	originServer            = "server"
	originFiles             = "files"
)

// fleetState is the server's working copy of the fleet: parsed profiles,
// what each agent reported, and resolved configs. Thousands of agents poll
// every half minute, so nothing here touches the database on the hot path.
type fleetState struct {
	mu       sync.Mutex
	profiles []*fleet.Profile
	rows     map[string]ProfileRow
	attrs    map[string]map[string]string // agent id -> labels overlaid with facts
	labels   map[string]map[string]string
	facts    map[string]map[string]string
	reports  map[string]string // agent id -> last hash+error stored
	resolved map[string]*resolveResult

	catalog   fleet.Catalog               // built-in integrations overlaid with this server's own
	custom    map[string]string           // documents of this server's own integrations
	inventory map[string]*fleet.Inventory // what runs on each agent's host
	detected  map[string][]string         // agent id -> integrations its inventory shows

	rollouts map[string]*rollout     // profile -> the rollout of its latest change
	isStable map[*fleet.Profile]bool // the earlier versions kept for those rollouts
	polls    map[string]uint64       // agent id -> how often it has polled since the server started
	seen     map[string]time.Time    // agent id -> its last poll
}

// setAttrs recomputes what selectors see for one agent: its labels and
// facts, plus detected_<integration>="true" for everything found running on
// it, so a profile can say "wherever nginx runs". Called with f.mu held.
func (f *fleetState) setAttrs(id string) {
	attrs := fleet.Attributes(id, f.labels[id], f.facts[id])
	f.detected[id] = f.catalog.Detect(f.inventory[id])
	for _, name := range f.detected[id] {
		attrs["detected_"+name] = "true"
	}
	f.attrs[id] = attrs
}

type resolveResult struct {
	r   *fleet.Resolved
	err error
}

// FleetAgent is an agent as the Fleet page shows it.
type FleetAgent struct {
	AgentInfo
	HasToken bool              `json:"has_token"`
	Labels   map[string]string `json:"labels"`
	Facts    map[string]string `json:"facts"`
	Profiles []string          `json:"profiles"`
	// Integrations are switched on for the agent; Detected were found
	// running on it; Unmonitored are detected but not switched on.
	Integrations []string `json:"integrations"`
	Detected     []string `json:"detected"`
	Unmonitored  []string `json:"unmonitored"`
	// ConfigSource is "managed" (profiles apply) or "local" (none do; the
	// agent uses its own file).
	ConfigSource string `json:"config_source"`
	// ConfigState is "in sync", "pending" (not picked up yet) or "error".
	ConfigState string `json:"config_state"`
	ConfigError string `json:"config_error,omitempty"`
	// Limited names what the server is holding this agent back on
	// ("metrics", "logs"): it sent more than the server's limits allow.
	Limited []string `json:"limited,omitempty"`
	// Rollouts names the profiles whose change this agent got ahead of the others.
	Rollouts []string `json:"rollouts,omitempty"`
}

// loadFleet reads profiles and agent attributes into memory, migrating
// configs saved before profiles existed.
func (s *Server) loadFleet() error {
	if done, _ := s.store.Setting(settingProfilesMigrated, ""); done == "" {
		old, err := s.store.AgentConfigs()
		if err != nil {
			return err
		}
		for _, c := range old {
			name, head := "agent-"+strings.ToLower(c.Target), fmt.Sprintf("select: ['agent=%q']\npriority: 1000\n", c.Target)
			if c.Target == "*" {
				name, head = "default", "priority: 0\n"
			}
			doc := head + "config:\n  " + strings.ReplaceAll(strings.TrimRight(c.YAML, "\n"), "\n", "\n  ") + "\n"
			if _, err := fleet.ParseProfile(name, doc); err != nil {
				s.log.Warn("old managed config not carried over to a profile", "target", c.Target, "err", err)
				continue
			}
			if _, err := s.store.PutProfile(name, doc, originServer, "migration"); err != nil {
				return err
			}
		}
		if err := s.store.SetSetting(settingProfilesMigrated, "1"); err != nil {
			return err
		}
	}
	f := &s.fleet
	f.reports, f.attrs = map[string]string{}, map[string]map[string]string{}
	f.labels, f.facts = map[string]map[string]string{}, map[string]map[string]string{}
	f.inventory, f.detected = map[string]*fleet.Inventory{}, map[string][]string{}
	f.rollouts, f.isStable = map[string]*rollout{}, map[*fleet.Profile]bool{}
	f.polls, f.seen = map[string]uint64{}, map[string]time.Time{}
	if err := s.reloadCatalog(); err != nil {
		return err
	}
	stored, err := s.store.AgentAttrs()
	if err != nil {
		return err
	}
	inventories, err := s.store.Inventories()
	if err != nil {
		return err
	}
	for id, lf := range stored {
		var labels, fs map[string]string
		json.Unmarshal([]byte(lf[0]), &labels)
		json.Unmarshal([]byte(lf[1]), &fs)
		f.labels[id], f.facts[id] = labels, fs
		if doc, ok := inventories[id]; ok {
			inv := &fleet.Inventory{}
			if json.Unmarshal([]byte(doc), inv) == nil {
				f.inventory[id] = inv
			}
		}
		f.setAttrs(id)
	}
	if err := s.reloadProfiles(); err != nil {
		return err
	}
	return s.loadRollouts()
}

// reloadCatalog rebuilds the catalog: the built-in integrations, replaced
// or extended by this server's own.
func (s *Server) reloadCatalog() error {
	cat, err := fleet.Builtin()
	if err != nil {
		return err
	}
	custom, err := s.store.Integrations()
	if err != nil {
		return err
	}
	for name, doc := range custom {
		in, err := fleet.ParseIntegration(name, doc)
		if err != nil {
			s.log.Error("stored integration is invalid and is ignored", "integration", name, "err", err)
			continue
		}
		cat[name] = in
	}
	f := &s.fleet
	f.mu.Lock()
	f.catalog, f.custom, f.resolved = cat, custom, map[string]*resolveResult{}
	for id := range f.attrs {
		f.setAttrs(id)
	}
	f.mu.Unlock()
	return nil
}

// reloadProfiles re-reads the profiles after a change and forgets resolved
// configs.
func (s *Server) reloadProfiles() error {
	rows, err := s.store.Profiles()
	if err != nil {
		return err
	}
	var parsed []*fleet.Profile
	byName := map[string]ProfileRow{}
	for _, row := range rows {
		p, err := fleet.ParseProfile(row.Name, row.Doc)
		if err != nil {
			s.log.Error("stored profile is invalid and is ignored", "profile", row.Name, "err", err)
			continue
		}
		parsed = append(parsed, p)
		byName[row.Name] = row
	}
	s.fleet.mu.Lock()
	s.fleet.profiles, s.fleet.rows, s.fleet.resolved = parsed, byName, map[string]*resolveResult{}
	s.fleet.mu.Unlock()
	return nil
}

// resolveFor returns the config of an agent with the given attributes.
// Agents matched by the same profiles share one result.
func (f *fleetState) resolveFor(profiles []*fleet.Profile, cache map[string]*resolveResult, attrs map[string]string) (*fleet.Resolved, error) {
	return f.resolveWith(profiles, f.catalog, cache, attrs)
}

func (f *fleetState) resolveWith(profiles []*fleet.Profile, catalog fleet.Catalog, cache map[string]*resolveResult, attrs map[string]string) (*fleet.Resolved, error) {
	var key strings.Builder
	for _, p := range fleet.Matching(profiles, attrs) {
		key.WriteString(p.Name)
		if f.isStable[p] { // the earlier version of a profile being rolled out
			key.WriteByte('~')
		}
		key.WriteByte(0)
	}
	if res, ok := cache[key.String()]; ok {
		return res.r, res.err
	}
	r, err := fleet.Resolve(profiles, catalog, attrs)
	cache[key.String()] = &resolveResult{r, err}
	return r, err
}

func (s *Server) fleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents", s.viewer(s.handleAgents))
	mux.HandleFunc("GET /api/v1/agents/{id}/config", s.viewer(s.handleEffectiveConfig))
	mux.HandleFunc("DELETE /api/v1/agents/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.store.ForgetAgent(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.forgetTokens()
		f := &s.fleet
		f.mu.Lock()
		delete(f.attrs, id)
		delete(f.labels, id)
		delete(f.facts, id)
		delete(f.reports, id)
		delete(f.inventory, id)
		delete(f.detected, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	// Issue a token that only this agent id can use. It is shown once.
	mux.HandleFunc("POST /api/v1/agents/{id}/token", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !agentIDRe.MatchString(id) || id == selfAgentName {
			http.Error(w, "agent ids may use letters, digits and . _ -", http.StatusBadRequest)
			return
		}
		tok := "vgl_" + randomToken()
		if err := s.store.SetAgentToken(id, hashToken(tok)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.forgetTokens()
		writeJSON(w, map[string]string{"agent_id": id, "token": tok})
	}))
	mux.HandleFunc("DELETE /api/v1/agents/{id}/token", s.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DeleteAgentToken(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.forgetTokens()
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /api/v1/fleet/profiles", s.viewer(s.handleProfiles))
	mux.HandleFunc("GET /api/v1/fleet/profiles/{name}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		s.fleet.mu.Lock()
		row, ok := s.fleet.rows[r.PathValue("name")]
		s.fleet.mu.Unlock()
		if !ok {
			http.Error(w, "no such profile", http.StatusNotFound)
			return
		}
		row.Doc = forViewer(r, row.Doc)
		writeJSON(w, row)
	}))
	mux.HandleFunc("PUT /api/v1/fleet/profiles/{name}", s.admin(s.putProfile))
	mux.HandleFunc("DELETE /api/v1/fleet/profiles/{name}", s.admin(s.deleteProfile))
	mux.HandleFunc("GET /api/v1/fleet/profiles/{name}/versions", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.store.ProfileVersions(r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("GET /api/v1/fleet/profiles/{name}/versions/{version}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.PathValue("version"))
		doc, found, err := s.store.ProfileVersion(r.PathValue("name"), v)
		if err != nil || !found {
			http.Error(w, "no such version", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		io.WriteString(w, forViewer(r, doc))
	}))
	// Which agents would a selector pick? For the editor, before saving.
	mux.HandleFunc("POST /api/v1/fleet/match", s.viewer(s.handleMatch))
	s.rolloutRoutes(mux)
	s.integrationRoutes(mux)
	s.containerLogRoutes(mux)

	// Agents report who they are and fetch their config here.
	mux.HandleFunc("POST /api/v1/agent/config", s.ingest(s.handleAgentConfig))
}

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	agents, err := s.store.Agents(now, s.cfg.HeartbeatTimeout.Std())
	var tokens map[string]bool
	var reports map[string][2]string
	if err == nil {
		tokens, reports, err = s.store.fleetMaps()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FleetAgent, 0, len(agents))
	for _, a := range agents {
		fa := FleetAgent{AgentInfo: a, HasToken: tokens[a.ID], Labels: f.labels[a.ID], Facts: f.facts[a.ID],
			Profiles: []string{}, Integrations: []string{}, Detected: f.detected[a.ID], Unmonitored: []string{}, ConfigSource: "local"}
		if fa.Detected == nil {
			fa.Detected = []string{}
		}
		attrs := f.attrs[a.ID]
		if attrs == nil { // never polled: all we know is its id
			attrs = fleet.Attributes(a.ID, nil, nil)
		}
		rep := reports[a.ID]
		want := ""
		res, rerr := f.current(a.ID, attrs)
		switch {
		case rerr != nil:
			fa.ConfigState, fa.ConfigError = "error", rerr.Error()
		case rep[1] != "":
			fa.ConfigState, fa.ConfigError = "error", rep[1]
		}
		if res != nil {
			want = res.Hash
			if len(res.Profiles) > 0 {
				fa.Profiles, fa.ConfigSource = res.Profiles, "managed"
			}
			if res.Integrations != nil {
				fa.Integrations = res.Integrations
			}
		}
		for _, signal := range []string{signalMetrics, signalLogs} {
			if _, recent := s.limiter.state(a.ID, signal, now); recent {
				fa.Limited = append(fa.Limited, signal)
			}
		}
		for name, ro := range f.rollouts {
			if ro.in[a.ID] && slices.Contains(fa.Profiles, name) {
				fa.Rollouts = append(fa.Rollouts, name)
			}
		}
		sort.Strings(fa.Rollouts)
		for _, name := range fa.Detected {
			if !slices.Contains(fa.Integrations, name) {
				fa.Unmonitored = append(fa.Unmonitored, name)
			}
		}
		if fa.ConfigState == "" {
			fa.ConfigState = "pending"
			if rep[0] == want {
				fa.ConfigState = "in sync"
			}
		}
		out = append(out, fa)
	}
	writeJSON(w, out)
}

// handleEffectiveConfig shows the config an agent gets and which profile
// each part of it comes from.
func (s *Server) handleEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f := &s.fleet
	f.mu.Lock()
	attrs := f.attrs[id]
	if attrs == nil {
		attrs = fleet.Attributes(id, nil, nil)
	}
	res, err := f.current(id, attrs)
	f.mu.Unlock()
	out := map[string]any{"source": "local", "attributes": attrs}
	if err != nil {
		out["error"] = err.Error()
	} else if len(res.Profiles) > 0 {
		out["source"], out["yaml"], out["hash"], out["profiles"], out["origins"] = "managed", forViewer(r, res.YAML), res.Hash, res.Profiles, res.Origins
		out["integrations"] = res.Integrations
	}
	writeJSON(w, out)
}

func (s *Server) handleProfiles(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ProfileRow
		Select   []string `json:"select"`
		Priority int      `json:"priority"`
		Matches  int      `json:"matches"`
	}
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]item, 0, len(f.profiles))
	for _, p := range f.profiles {
		row := f.rows[p.Name]
		row.Doc = ""
		it := item{ProfileRow: row, Select: p.Select, Priority: p.Priority}
		if it.Select == nil {
			it.Select = []string{}
		}
		for _, attrs := range f.attrs {
			if p.Matches(attrs) {
				it.Matches++
			}
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, out)
}

// secretRe finds the values of settings that hold credentials: what an
// agent uses to scrape a target or to talk to a service.
var secretRe = regexp.MustCompile(`(?i)\b(bearer_token|password|passwd|secret|api_key|token)(["']?\s*:\s*)("[^"\n]*"|'[^'\n]*'|[^\s,}#"'][^,}\n#]*)`)

// forViewer returns configuration text as the caller may see it. Profiles
// hold credentials; an account that may only look gets them blanked out.
func forViewer(r *http.Request, text string) string {
	if p, ok := r.Context().Value(principalKey{}).(*principal); ok && p.Role == RoleAdmin {
		return text
	}
	return secretRe.ReplaceAllString(text, "${1}${2}(hidden)")
}

func author(r *http.Request) string {
	if p, ok := r.Context().Value(principalKey{}).(*principal); ok {
		return p.User
	}
	return "unknown"
}

// putProfile saves a profile if, with it, every known agent still gets a
// valid config. Otherwise nothing changes and the reason names an agent.
func (s *Server) putProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	text, ok := readConfigBody(w, r)
	if !ok {
		return
	}
	p, err := fleet.ParseProfile(name, text)
	if err != nil {
		http.Error(w, "invalid profile: "+err.Error(), http.StatusBadRequest)
		return
	}
	f := &s.fleet
	f.mu.Lock()
	candidate := []*fleet.Profile{p}
	for _, old := range f.profiles {
		if old.Name != name {
			candidate = append(candidate, old)
		}
	}
	err = checkIntegrations(p, f.catalog)
	if err == nil {
		err = checkAgainstAgents(f, candidate, f.catalog)
	}
	f.mu.Unlock()
	if err != nil {
		http.Error(w, "profile not saved: "+err.Error(), http.StatusBadRequest)
		return
	}
	origin := originServer
	if r.Header.Get("X-Vigil-Origin") == originFiles {
		origin = originFiles
	}
	// A profile with a rollout section spreads its changes in steps, unless
	// this save says otherwise. The rollout is registered before the save,
	// so that no agent sees the new text before it is admitted.
	staged := p.Rollout != nil && r.URL.Query().Get("rollout") != "skip"
	now := time.Now()
	s.rolloutMu.Lock()
	defer s.rolloutMu.Unlock()
	f.mu.Lock()
	_, was := f.rollouts[name]
	var ro *rollout
	if staged {
		ro = f.beginRollout(p, author(r), now)
	}
	f.mu.Unlock()
	v, err := s.store.PutProfile(name, text, origin, author(r))
	if err != nil {
		if staged && !was {
			f.mu.Lock()
			f.endRollout(name)
			f.mu.Unlock()
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if staged {
		f.mu.Lock()
		ro.To = v
		doc, _ := json.Marshal(ro)
		f.mu.Unlock()
		err = s.store.SetRollout(name, string(doc))
	} else if was {
		// Saved for everyone at once: the rollout that was running is over.
		f.mu.Lock()
		f.endRollout(name)
		f.mu.Unlock()
		err = s.store.DeleteRollout(name)
	}
	if err == nil {
		err = s.reloadProfiles()
	}
	if err == nil {
		err = s.syncIntegrations()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if staged {
		s.stepRollouts(now) // admit the first agents right away
	}
	f.mu.Lock()
	_, staged = f.rollouts[name] // a change that affects nobody is over at once
	f.mu.Unlock()
	writeJSON(w, map[string]any{"name": name, "version": v, "rollout": staged})
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f := &s.fleet
	f.mu.Lock()
	var candidate []*fleet.Profile
	for _, old := range f.profiles {
		if old.Name != name {
			candidate = append(candidate, old)
		}
	}
	err := checkAgainstAgents(f, candidate, f.catalog)
	f.mu.Unlock()
	if err != nil {
		http.Error(w, "profile not deleted: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.rolloutMu.Lock()
	defer s.rolloutMu.Unlock()
	if err = s.store.DeleteProfile(name, author(r)); err == nil {
		err = s.reloadProfiles()
	}
	if err == nil { // deleting is not staged, and ends a rollout of the profile
		f.mu.Lock()
		f.endRollout(name)
		f.mu.Unlock()
		err = s.store.DeleteRollout(name)
	}
	if err == nil {
		err = s.syncIntegrations()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkAgainstAgents resolves a candidate set of profiles for every known
// agent. Called with f.mu held.
func checkAgainstAgents(f *fleetState, candidate []*fleet.Profile, catalog fleet.Catalog) error {
	cache := map[string]*resolveResult{}
	var firstErr error
	var example string
	broken := 0
	for id, attrs := range f.attrs {
		if _, err := f.resolveWith(candidate, catalog, cache, attrs); err != nil {
			broken++
			if firstErr == nil || id < example {
				firstErr, example = err, id
			}
		}
	}
	if firstErr == nil {
		return nil
	}
	if broken > 1 {
		return fmt.Errorf("agent %s and %d more would get an invalid config: %w", example, broken-1, firstErr)
	}
	return fmt.Errorf("agent %s would get an invalid config: %w", example, firstErr)
}

// handleMatch previews a profile without saving it: which agents its
// selector picks, and whether every agent would still get a valid config.
// It takes either a whole profile document or just a selector.
func (s *Server) handleMatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		Doc    string   `json:"doc"`
		Select []string `json:"select"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" {
		in.Name = "preview"
	}
	if in.Doc == "" {
		doc, _ := json.Marshal(map[string]any{"select": in.Select}) // JSON is YAML
		in.Doc = string(doc)
	}
	p, err := fleet.ParseProfile(in.Name, in.Doc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := &s.fleet
	f.mu.Lock()
	ids := []string{} // [] rather than null when nothing matches: the editor reads its length
	for id, attrs := range f.attrs {
		if p.Matches(attrs) {
			ids = append(ids, id)
		}
	}
	candidate := []*fleet.Profile{p}
	for _, old := range f.profiles {
		if old.Name != in.Name {
			candidate = append(candidate, old)
		}
	}
	problem := ""
	if err := checkIntegrations(p, f.catalog); err != nil {
		problem = err.Error()
	} else if err := checkAgainstAgents(f, candidate, f.catalog); err != nil {
		problem = err.Error()
	}
	total := len(f.attrs)
	f.mu.Unlock()
	sort.Strings(ids)
	matched := len(ids)
	if len(ids) > 50 {
		ids = ids[:50]
	}
	writeJSON(w, map[string]any{"matched": matched, "of": total, "agents": ids, "problem": problem})
}

// handleAgentConfig answers an agent's poll. The agent says who it is
// (labels, facts) and what it runs (hash, error); the answer is 200 with its
// config, 304 if it already has it, or 204 if no profile applies to it.
func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := agentID(w, r)
	if !ok {
		return
	}
	var in struct {
		Labels      map[string]string `json:"labels"`
		Facts       map[string]string `json:"facts"`
		ConfigHash  string            `json:"config_hash"`
		ConfigError string            `json:"config_error"`
		// Inventory is sent only when it changed since the agent last sent it.
		Inventory *fleet.Inventory `json:"inventory"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid report: "+err.Error(), http.StatusBadRequest)
		return
	}
	// A label may not use a fact's name; the agent's own id is not negotiable.
	for _, name := range facts.Names {
		delete(in.Labels, name)
	}
	if len(in.ConfigError) > 500 {
		in.ConfigError = in.ConfigError[:500]
	}
	labelsJSON, _ := json.Marshal(in.Labels)
	factsJSON, _ := json.Marshal(in.Facts)
	attrKey, report := string(labelsJSON)+string(factsJSON), in.ConfigHash+"\x00"+in.ConfigError

	f := &s.fleet
	f.mu.Lock()
	old, _ := json.Marshal(f.labels[id])
	oldFacts, _ := json.Marshal(f.facts[id])
	attrsChanged := f.attrs[id] == nil || string(old)+string(oldFacts) != attrKey
	reportChanged := f.reports[id] != report
	f.labels[id], f.facts[id], f.reports[id] = in.Labels, in.Facts, report
	if in.Inventory != nil {
		f.inventory[id] = in.Inventory
	}
	wantInventory := f.inventory[id] == nil
	f.setAttrs(id)
	f.polls[id]++
	f.seen[id] = time.Now()
	res, rerr := f.current(id, f.attrs[id])
	f.mu.Unlock()

	if in.Inventory != nil {
		doc, _ := json.Marshal(in.Inventory)
		if err := s.store.SetInventory(id, string(doc)); err != nil {
			s.log.Error("storing agent inventory failed", "agent", id, "err", err)
		}
	}
	if wantInventory {
		w.Header().Set("X-Vigil-Want-Inventory", "1")
	}

	// Only what changed is written, so a steady fleet costs no disk writes.
	if attrsChanged {
		if err := s.store.SetAgentAttrs(id, string(labelsJSON), string(factsJSON)); err != nil {
			s.log.Error("storing agent attributes failed", "agent", id, "err", err)
		}
	}
	if reportChanged {
		if err := s.store.ReportAgentConfig(id, in.ConfigHash, in.ConfigError); err != nil {
			s.log.Error("recording agent config report failed", "agent", id, "err", err)
		}
	}
	if rerr != nil {
		// The profiles do not add up for this agent. It keeps what it has;
		// the Fleet page shows the reason.
		http.Error(w, rerr.Error(), http.StatusConflict)
		return
	}
	if len(res.Profiles) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	etag := `"` + res.Hash + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	io.WriteString(w, res.YAML)
}
