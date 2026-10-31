package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ijcodelabs/vigil/internal/rules"
	"github.com/ijcodelabs/vigil/internal/slo"
)

// Service level objectives. Each is a YAML document kept with its earlier
// versions, like a profile, and compiled into a rule group "slo:<name>"
// that runs next to the rule file (applyRules). Everyone signed in sees
// them; admins change them. The spec is memory/context/slo.md.

// defaultTenant is the only tenant there is so far. The SLO tables carry
// the column so that more can be added without changing their keys.
const defaultTenant = "default"

// SLORow is an SLO as the API shows it: the parsed document plus where it
// came from.
type SLORow struct {
	*slo.SLO
	Kind    string    `json:"kind"`
	Doc     string    `json:"doc,omitempty"`
	Version int       `json:"version"`
	Origin  string    `json:"origin"` // "server" (UI or API) or "files" (vigil apply)
	Author  string    `json:"author"`
	Updated time.Time `json:"updated"`
	Deleted bool      `json:"deleted,omitempty"` // only in version history
	// Error is set when a stored document no longer parses (a newer
	// server's field, say); such an SLO records nothing until it is saved
	// again.
	Error string `json:"error,omitempty"`
}

func (s *Server) sloRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/slos", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.store.SLOs(defaultTenant)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for i := range list {
			list[i].Doc = ""
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("GET /api/v1/slos/{name}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		row, found, err := s.store.SLO(defaultTenant, r.PathValue("name"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "no such SLO", http.StatusNotFound)
		default:
			out := map[string]any{"slo": row}
			if row.Error == "" {
				out["rules"] = row.SLO.RuleGroups()
			}
			writeJSON(w, out)
		}
	}))
	mux.HandleFunc("PUT /api/v1/slos/{name}", s.admin(s.putSLO))
	mux.HandleFunc("DELETE /api/v1/slos/{name}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		found, err := s.store.DeleteSLO(defaultTenant, r.PathValue("name"), author(r))
		if err == nil && found {
			err = s.reapplyRules()
		}
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "no such SLO", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	mux.HandleFunc("GET /api/v1/slos/{name}/versions", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.store.SLOVersions(defaultTenant, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("GET /api/v1/slos/{name}/versions/{version}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		v, err := strconv.Atoi(r.PathValue("version"))
		if err != nil {
			http.Error(w, "version is a number", http.StatusBadRequest)
			return
		}
		doc, found, err := s.store.SLOVersion(defaultTenant, r.PathValue("name"), v)
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "no such version", http.StatusNotFound)
		default:
			writeJSON(w, map[string]any{"name": r.PathValue("name"), "version": v, "doc": doc})
		}
	}))
}

// putSLO saves an SLO document if it parses and the metric store accepts
// the queries made of it. An identical document is not saved again. The
// answer lists warnings that do not stop the save, such as no events yet.
func (s *Server) putSLO(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	text, ok := readConfigBody(w, r)
	if !ok {
		return
	}
	def, err := slo.Parse(name, text)
	if err != nil {
		http.Error(w, "SLO not saved: "+err.Error(), http.StatusBadRequest)
		return
	}
	groups := def.RuleGroups()
	if err := s.rules.Check(r.Context(), &rules.File{Groups: groups}); err != nil {
		var qe *rules.QueryError
		if errors.As(err, &qe) && qe.Bad {
			http.Error(w, "SLO not saved, the metric store refuses its queries: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "SLO not saved, its queries could not be checked: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	warnings := append([]string{}, def.Warnings()...)
	if series, err := s.rules.Query(r.Context(), rules.TypeMetrics, def.EventsQuery(time.Hour), time.Now()); err == nil && len(series) == 0 {
		warnings = append(warnings, "no events in the last hour from "+def.Source()+"; check the name. The SLO counts from when there are some")
	}
	for _, g := range groups {
		if s.ruleFileHas(g.Name) {
			warnings = append(warnings, fmt.Sprintf("the rule file has a group called %q, which runs instead of this SLO's", g.Name))
		}
	}
	cur, found, err := s.store.SLO(defaultTenant, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, changed := cur.Version, !found || cur.Doc != text
	if changed {
		if v, err = s.store.PutSLO(defaultTenant, name, text, origin(r), author(r)); err == nil {
			err = s.reapplyRules()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"name": name, "version": v, "changed": changed, "warnings": warnings})
}

// reapplyRules restarts rule evaluation with the current SLOs, once it
// runs at all.
func (s *Server) reapplyRules() error {
	if s.rulesStarted.Load() {
		return s.applyRules(s.runCtx)
	}
	return nil
}

func (s *Server) ruleFileHas(group string) bool {
	text, err := s.store.Setting(settingRules, "")
	if err != nil {
		return false
	}
	f, err := rules.Parse(text)
	if err != nil {
		return false
	}
	for _, g := range f.Groups {
		if g.Name == group {
			return true
		}
	}
	return false
}

// sloRuleGroups are the rule groups of every SLO that parses. One that
// does not is logged and left out; the others run.
func (s *Server) sloRuleGroups() []rules.Group {
	list, err := s.store.SLOs(defaultTenant)
	if err != nil {
		s.log.Warn("SLOs not loaded", "err", err)
		return nil
	}
	var out []rules.Group
	for _, row := range list {
		if row.Error != "" {
			s.log.Warn("SLO skipped: its document does not parse", "slo", row.Name, "err", row.Error)
			continue
		}
		out = append(out, row.SLO.RuleGroups()...)
	}
	return out
}

func scanSLO(row *SLORow, name string, at int64) {
	row.Updated = time.Unix(at, 0).UTC()
	def, err := slo.Parse(name, row.Doc)
	if err != nil {
		row.SLO, row.Error = &slo.SLO{Name: name}, err.Error()
		return
	}
	row.SLO, row.Kind = def, def.Kind()
}

func (s *Store) SLOs(tenant string) ([]SLORow, error) {
	rows, err := s.db.Query(`SELECT name, doc, version, origin, author, updated FROM slos WHERE tenant = ? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SLORow{}
	for rows.Next() {
		var row SLORow
		var name string
		var at int64
		if err := rows.Scan(&name, &row.Doc, &row.Version, &row.Origin, &row.Author, &at); err != nil {
			return nil, err
		}
		scanSLO(&row, name, at)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) SLO(tenant, name string) (SLORow, bool, error) {
	var row SLORow
	var at int64
	err := s.db.QueryRow(`SELECT doc, version, origin, author, updated FROM slos WHERE tenant = ? AND name = ?`, tenant, name).
		Scan(&row.Doc, &row.Version, &row.Origin, &row.Author, &at)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	scanSLO(&row, name, at)
	return row, true, nil
}

func nextSLOVersion(tx *sql.Tx, tenant, name string) (int, error) {
	var v sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(version) FROM slo_versions WHERE tenant = ? AND name = ?`, tenant, name).Scan(&v)
	return int(v.Int64) + 1, err
}

// PutSLO saves an SLO as a new version and returns its number.
func (s *Store) PutSLO(tenant, name, doc, origin, author string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	v, err := nextSLOVersion(tx, tenant, name)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO slo_versions (tenant, name, version, doc, origin, author, time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tenant, name, v, doc, origin, author, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO slos (tenant, name, doc, version, origin, author, updated) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, name) DO UPDATE SET doc = excluded.doc, version = excluded.version, origin = excluded.origin,
		author = excluded.author, updated = excluded.updated`, tenant, name, doc, v, origin, author, now); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// DeleteSLO removes an SLO; the deletion is recorded in its history. The
// series it recorded stay in the metric store until its retention ends.
func (s *Store) DeleteSLO(tenant, name, author string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM slos WHERE tenant = ? AND name = ?`, tenant, name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	v, err := nextSLOVersion(tx, tenant, name)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO slo_versions (tenant, name, version, doc, origin, author, time, deleted) VALUES (?, ?, ?, '', 'server', ?, ?, 1)`,
		tenant, name, v, author, time.Now().Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SLOVersions lists an SLO's history, newest first, without the text.
func (s *Store) SLOVersions(tenant, name string) ([]ProfileRow, error) {
	rows, err := s.db.Query(`SELECT version, origin, author, time, deleted FROM slo_versions WHERE tenant = ? AND name = ? ORDER BY version DESC`, tenant, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileRow{}
	for rows.Next() {
		p := ProfileRow{Name: name}
		var at int64
		if err := rows.Scan(&p.Version, &p.Origin, &p.Author, &at, &p.Deleted); err != nil {
			return nil, err
		}
		p.Updated = time.Unix(at, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SLOVersion(tenant, name string, version int) (doc string, found bool, err error) {
	err = s.db.QueryRow(`SELECT doc FROM slo_versions WHERE tenant = ? AND name = ? AND version = ? AND deleted = 0`, tenant, name, version).Scan(&doc)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return doc, err == nil, err
}
