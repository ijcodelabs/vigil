package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Provided dashboards come with vigil: the starter dashboards, and the
// dashboard of every integration in use. Each is installed once and belongs
// to the user from then on. When a newer vigil brings a newer version, a
// dashboard nobody changed is replaced with it; one that was changed is
// left alone and the newer version is offered; one that was deleted stays
// deleted.
//
// What was installed is remembered per dashboard as a fingerprint of that
// version, under settingProvided+id.
const (
	settingProvided = "dashboard_provided:"
	// providedLegacy marks a dashboard installed by a version that did not
	// keep fingerprints: it cannot be told whether it was changed since.
	providedLegacy = "legacy"
)

type providedDashboard struct {
	source string // "starter", or "integration <name>"
	body   string // canonical JSON
}

// canonDashboard is a dashboard's JSON in one fixed form, so that two
// spellings of the same dashboard compare equal.
func canonDashboard(id string, body []byte) (string, string, error) {
	d, err := parseDashboard(body)
	if err != nil {
		return "", "", err
	}
	d.ID = id
	clean, _ := json.Marshal(d)
	return string(clean), d.Title, nil
}

func fingerprint(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// provided lists what this version of vigil would install right now.
func (s *Server) provided() map[string]providedDashboard {
	out := map[string]providedDashboard{}
	entries, _ := starterDashboards.ReadDir("dashboards")
	for _, e := range entries {
		raw, err := starterDashboards.ReadFile("dashboards/" + e.Name())
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if body, _, err := canonDashboard(id, raw); err == nil {
			out[id] = providedDashboard{"starter", body}
		} else {
			s.log.Error("starter dashboard is invalid", "dashboard", id, "err", err)
		}
	}
	f := &s.fleet
	f.mu.Lock()
	used, catalog := f.usedIntegrations(), f.catalog
	f.mu.Unlock()
	for _, name := range used {
		in := catalog[name]
		if in == nil || in.Dashboard == nil {
			continue
		}
		raw, _ := json.Marshal(in.Dashboard)
		body, _, err := canonDashboard(name, raw)
		if err != nil {
			s.log.Error("integration dashboard is invalid and was not installed", "integration", name, "err", err)
			continue
		}
		out[name] = providedDashboard{"integration " + name, body} // an integration's dashboard wins over a starter of the same id
	}
	return out
}

// providedState returns the fingerprint recorded for a provided dashboard,
// reading what earlier versions recorded in their own way.
func (s *Server) providedState(id string, p providedDashboard) string {
	if v, _ := s.store.Setting(settingProvided+id, ""); v != "" {
		return v
	}
	if p.source == "starter" {
		seeded, _ := s.store.Setting(settingSeeded, "")
		if seeded == "1" {
			seeded = "containers,hosts,logs-and-security"
		}
		for _, x := range strings.Split(seeded, ",") {
			if x == id {
				return providedLegacy
			}
		}
		return ""
	}
	if v, _ := s.store.Setting(settingIntegrationDashboard+id, ""); v != "" {
		return providedLegacy
	}
	return ""
}

// syncProvided installs provided dashboards that are new, and updates the
// ones nobody has changed.
func (s *Server) syncProvided() error {
	for id, p := range s.provided() {
		state, want := s.providedState(id, p), fingerprint(p.body)
		if state == want {
			continue
		}
		stored, exists, err := s.store.Dashboard(id)
		if err != nil {
			return err
		}
		current := ""
		if exists {
			if c, _, err := canonDashboard(id, []byte(stored)); err == nil {
				current = c
			}
		}
		_, title, _ := canonDashboard(id, []byte(p.body))
		switch {
		case state == "" && !exists:
			// Never installed, and no dashboard of the user's in its place.
			if err := s.store.PutDashboard(id, title, p.body); err != nil {
				return err
			}
		case state == "":
			// The id is taken by a dashboard of the user's own: leave it, and do not offer.
		case !exists:
			// Deleted by the user: stays deleted.
		case current == p.body:
			// Already what we would install.
		case state != providedLegacy && fingerprint(current) == state:
			// Unchanged since it was installed: bring it up to date.
			if err := s.store.PutDashboard(id, title, p.body); err != nil {
				return err
			}
			s.log.Info("dashboard updated to the version that comes with this vigil", "dashboard", id)
		default:
			// Changed by the user (or installed before fingerprints were
			// kept): the newer version is offered, not imposed.
			continue
		}
		if err := s.store.SetSetting(settingProvided+id, want); err != nil {
			return err
		}
	}
	return nil
}

// providedUpdate reports whether a newer provided version is on offer for
// a dashboard, and what it is.
func (s *Server) providedUpdate(id string, all map[string]providedDashboard) (providedDashboard, bool) {
	p, ok := all[id]
	if !ok {
		return p, false
	}
	state := s.providedState(id, p)
	if state == "" || state == fingerprint(p.body) {
		return p, false
	}
	stored, exists, _ := s.store.Dashboard(id)
	if !exists {
		return p, false
	}
	current, _, _ := canonDashboard(id, []byte(stored))
	return p, current != p.body
}

func (s *Server) providedRoutes(mux *http.ServeMux) {
	// The newer version on offer, to look at before deciding.
	mux.HandleFunc("GET /api/v1/dashboards/{id}/update", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.providedUpdate(r.PathValue("id"), s.provided())
		if !ok {
			w.WriteHeader(http.StatusNoContent) // nothing on offer; the usual case, not an error
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, p.body)
	}))
	// {"action": "apply"} replaces the dashboard with the newer version;
	// with "copy": true the current one is kept as <id>-mine. "keep"
	// declines this version. "restore" puts a provided dashboard back that
	// was deleted.
	mux.HandleFunc("POST /api/v1/dashboards/{id}/update", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in struct {
			Action string `json:"action"`
			Copy   bool   `json:"copy"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		all := s.provided()
		p, offered := s.providedUpdate(id, all)
		stored, exists, err := s.store.Dashboard(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch {
		case in.Action == "restore" && !exists && p.body != "":
		case (in.Action == "apply" || in.Action == "keep") && offered:
		case in.Action != "apply" && in.Action != "keep" && in.Action != "restore":
			http.Error(w, "the action is apply, keep or restore", http.StatusBadRequest)
			return
		default:
			http.Error(w, "nothing to do: no newer version is on offer for this dashboard", http.StatusConflict)
			return
		}
		if in.Action != "keep" {
			if in.Copy && exists {
				mine, title, err := canonDashboard(id+"-mine", []byte(stored))
				if err == nil {
					var d Dashboard
					json.Unmarshal([]byte(mine), &d)
					d.Title = title + " (mine)"
					b, _ := json.Marshal(d)
					err = s.store.PutDashboard(id+"-mine", d.Title, string(b))
				}
				if err != nil {
					http.Error(w, "could not keep a copy, nothing was changed: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			_, title, _ := canonDashboard(id, []byte(p.body))
			if err := s.store.PutDashboard(id, title, p.body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := s.store.SetSetting(settingProvided+id, fingerprint(p.body)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
