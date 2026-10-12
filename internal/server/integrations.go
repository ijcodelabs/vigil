package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"

	"github.com/ijcodelabs/vigil/internal/fleet"
	"github.com/ijcodelabs/vigil/internal/rules"
)

const settingIntegrationDashboard = "integration_dashboard:"

// checkIntegrations makes sure a profile only names integrations that exist
// and only sets parameters they have, whether or not an agent matches yet.
func checkIntegrations(p *fleet.Profile, catalog fleet.Catalog) error {
	for name, params := range p.Integrations {
		in := catalog[name]
		if in == nil {
			return fmt.Errorf("integration %q does not exist (known: %v)", name, catalog.Names())
		}
		if _, err := in.Fragment(params); err != nil {
			return err
		}
	}
	return nil
}

// usedIntegrations lists the integrations some profile switches on.
// Called with f.mu held.
func (f *fleetState) usedIntegrations() []string {
	used := map[string]bool{}
	for _, p := range f.profiles {
		for name := range p.Integrations {
			if f.catalog[name] != nil {
				used[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(used))
}

// syncIntegrations brings the server-side parts of the integrations in use
// up to date: their dashboards are installed once, their rule groups run
// next to the user's own.
func (s *Server) syncIntegrations() error {
	if err := s.syncProvided(); err != nil {
		return err
	}
	if s.rulesStarted.Load() {
		return s.applyRules(s.runCtx)
	}
	return nil
}

// applyRules starts evaluating the saved rule file together with the rule
// groups of the integrations in use.
func (s *Server) applyRules(ctx context.Context) error {
	text, err := s.store.Setting(settingRules, "")
	if err != nil {
		return err
	}
	file, err := rules.Parse(text)
	if err != nil {
		return fmt.Errorf("saved rules: %w", err)
	}
	taken := map[string]bool{}
	for _, g := range file.Groups {
		taken[g.Name] = true
	}
	if g, ok := builtinRuleGroup(file); ok {
		file.Groups = append(file.Groups, g)
		taken[g.Name] = true
	}
	f := &s.fleet
	f.mu.Lock()
	used, catalog := f.usedIntegrations(), f.catalog
	f.mu.Unlock()
	for _, name := range used {
		groups, err := catalog[name].RuleGroups()
		if err != nil {
			continue
		}
		for _, g := range groups {
			if taken[g.Name] {
				s.log.Warn("integration rule group skipped: the rule file has a group of the same name", "group", g.Name)
				continue
			}
			file.Groups = append(file.Groups, g)
		}
	}
	s.rulesStarted.Store(true)
	return s.rules.Apply(ctx, file)
}

func (s *Server) integrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/fleet/integrations", s.viewer(s.handleIntegrations))
	mux.HandleFunc("GET /api/v1/fleet/integrations/{name}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		f := &s.fleet
		f.mu.Lock()
		doc, custom := f.custom[name]
		f.mu.Unlock()
		builtin, isBuiltin := fleet.BuiltinText(name)
		if !custom && !isBuiltin {
			http.Error(w, "no such integration", http.StatusNotFound)
			return
		}
		if !custom {
			doc = builtin
		}
		writeJSON(w, map[string]any{"name": name, "doc": doc, "builtin": isBuiltin, "customised": custom && isBuiltin})
	}))
	mux.HandleFunc("PUT /api/v1/fleet/integrations/{name}", s.admin(s.putIntegration))
	mux.HandleFunc("DELETE /api/v1/fleet/integrations/{name}", s.admin(s.deleteIntegration))
}

func (s *Server) handleIntegrations(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		*fleet.Integration
		Customised        bool     `json:"customised"` // a built-in one changed on this server
		HasRules          bool     `json:"has_rules"`
		HasDashboard      bool     `json:"has_dashboard"`
		Profiles          []string `json:"profiles"`           // profiles that switch it on
		Detected          int      `json:"detected"`           // agents it was found running on
		Enabled           int      `json:"enabled"`            // agents it is switched on for
		Unmonitored       int      `json:"unmonitored"`        // detected but not switched on
		UnmonitoredAgents []string `json:"unmonitored_agents"` // the first few of those
	}
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	enabled := map[string]map[string]bool{} // integration -> agents
	for id, attrs := range f.attrs {
		res, err := f.current(id, attrs)
		if err != nil || res == nil {
			continue
		}
		for _, name := range res.Integrations {
			if enabled[name] == nil {
				enabled[name] = map[string]bool{}
			}
			enabled[name][id] = true
		}
	}
	out := []item{}
	for _, name := range f.catalog.Names() {
		in := f.catalog[name]
		_, custom := f.custom[name]
		_, isBuiltin := fleet.BuiltinText(name)
		it := item{Integration: in, Customised: custom && isBuiltin, HasRules: len(in.Rules) > 0, HasDashboard: in.Dashboard != nil,
			Profiles: []string{}, Enabled: len(enabled[name]), UnmonitoredAgents: []string{}}
		it.Builtin = isBuiltin
		for _, p := range f.profiles {
			if _, ok := p.Integrations[name]; ok {
				it.Profiles = append(it.Profiles, p.Name)
			}
		}
		for id, found := range f.detected {
			if !slices.Contains(found, name) {
				continue
			}
			it.Detected++
			if !enabled[name][id] {
				it.Unmonitored++
				it.UnmonitoredAgents = append(it.UnmonitoredAgents, id)
			}
		}
		sort.Strings(it.UnmonitoredAgents)
		if len(it.UnmonitoredAgents) > 20 {
			it.UnmonitoredAgents = it.UnmonitoredAgents[:20]
		}
		out = append(out, it)
	}
	writeJSON(w, out)
}

// putIntegration adds an integration, or replaces a built-in one on this
// server. It is refused if it would leave an agent with an invalid config.
func (s *Server) putIntegration(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	text, ok := readConfigBody(w, r)
	if !ok {
		return
	}
	in, err := fleet.ParseIntegration(name, text)
	if err == nil && in.Dashboard != nil {
		body, _ := json.Marshal(in.Dashboard)
		if _, derr := parseDashboard(body); derr != nil {
			err = fmt.Errorf("dashboard: %w", derr)
		}
	}
	if err != nil {
		http.Error(w, "invalid integration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.changeCatalog(name, in); err != nil {
		http.Error(w, "integration not saved: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.PutIntegration(name, text, author(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.afterCatalogChange(w)
}

// deleteIntegration removes this server's own integration, or its changes
// to a built-in one, which then applies again as shipped.
func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.fleet.mu.Lock()
	_, custom := s.fleet.custom[name]
	s.fleet.mu.Unlock()
	if !custom {
		http.Error(w, "this integration is built in and cannot be deleted; a customised copy can", http.StatusBadRequest)
		return
	}
	builtin, _ := fleet.Builtin()
	if err := s.changeCatalog(name, builtin[name]); err != nil {
		http.Error(w, "integration not deleted: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteIntegration(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.afterCatalogChange(w)
}

// changeCatalog checks what would happen with integration `name` replaced
// by in (nil: removed): every profile must still name existing integrations
// and every agent must still get a valid config.
func (s *Server) changeCatalog(name string, in *fleet.Integration) error {
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	candidate := maps.Clone(f.catalog)
	if in == nil {
		delete(candidate, name)
	} else {
		candidate[name] = in
	}
	for _, p := range f.profiles {
		if err := checkIntegrations(p, candidate); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	return checkAgainstAgents(f, f.profiles, candidate)
}

func (s *Server) afterCatalogChange(w http.ResponseWriter) {
	err := s.reloadCatalog()
	if err == nil {
		err = s.syncIntegrations()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
