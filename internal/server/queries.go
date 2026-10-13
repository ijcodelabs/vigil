package server

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/fleet"
)

// Saved queries are what Explore offers under "Saved": a name for a PromQL
// or LogsQL query, so it is picked instead of typed again.
//
// One is either personal (only its owner sees and changes it; a viewer may
// have them too) or shared (everyone sees it, admins change it). Next to
// those the list holds the queries that come with vigil: the starter ones
// and those of the integrations in use. These are not stored and cannot be
// changed here; they change with the version, or with the integration.

//go:embed starter_queries.yaml
var starterQueriesText string

var starterQueries = sync.OnceValue(func() []fleet.Query {
	list, err := fleet.ParseQueries(starterQueriesText)
	if err != nil {
		panic(fmt.Sprintf("starter_queries.yaml: %v", err)) // a build mistake; the test catches it
	}
	return list
})

const (
	maxOwnQueries    = 200
	maxSharedQueries = 1000
)

// SavedQuery is one entry of the list.
type SavedQuery struct {
	ID string `json:"id,omitempty"`
	fleet.Query
	Owner   string     `json:"owner,omitempty"` // who saved it, or who shared it
	Shared  bool       `json:"shared"`
	Origin  string     `json:"origin,omitempty"` // "server" (the interface or the API) or "files" (vigil apply)
	Updated *time.Time `json:"updated,omitempty"`
	// Source says where a query that comes with vigil is from: "starter"
	// or "integration <name>". Empty for the saved ones.
	Source string `json:"source,omitempty"`
	// Editable tells the interface whether this caller may change it.
	Editable bool `json:"editable"`
}

// providedQueries lists what this vigil offers without anyone saving it.
func (s *Server) providedQueries() []SavedQuery {
	out := []SavedQuery{}
	for _, q := range starterQueries() {
		out = append(out, SavedQuery{Query: q, Source: "starter"})
	}
	f := &s.fleet
	f.mu.Lock()
	used, catalog := f.usedIntegrations(), f.catalog
	f.mu.Unlock()
	for _, name := range used {
		for _, q := range catalog[name].Queries {
			out = append(out, SavedQuery{Query: q, Source: "integration " + name})
		}
	}
	return out
}

func (s *Server) queryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/queries", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		p := caller(r)
		list, err := s.store.Queries(p.User)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for i := range list {
			list[i].Editable = !list[i].Shared || p.Role == RoleAdmin
		}
		writeJSON(w, append(list, s.providedQueries()...))
	}))
	// Saving is open to viewers: a personal query changes nothing for
	// anyone else. Sharing one is an admin's decision.
	mux.HandleFunc("POST /api/v1/queries", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			fleet.Query
			Shared bool `json:"shared"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		p := caller(r)
		q := SavedQuery{ID: randomToken()[:16], Query: in.Query, Owner: p.User, Shared: in.Shared, Origin: origin(r)}
		if !s.checkQuery(w, p, &q) {
			return
		}
		s.saveQuery(w, &q)
	}))
	// Only the fields given are changed. A query keeps its type.
	mux.HandleFunc("PUT /api/v1/queries/{id}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		p := caller(r)
		q, ok := s.ownQuery(w, r, p)
		if !ok {
			return
		}
		var in struct {
			Name   *string `json:"name"`
			Query  *string `json:"query"`
			Shared *bool   `json:"shared"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Name != nil {
			q.Name = *in.Name
		}
		if in.Query != nil {
			q.Query.Query = *in.Query
		}
		if in.Shared != nil && *in.Shared != q.Shared {
			// Sharing, and taking one back, is signed by who did it: a
			// query that stops being shared becomes that admin's own.
			q.Shared, q.Owner = *in.Shared, p.User
		}
		q.Origin = origin(r)
		if !s.checkQuery(w, p, &q) {
			return
		}
		s.saveQuery(w, &q)
	}))
	mux.HandleFunc("DELETE /api/v1/queries/{id}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		q, ok := s.ownQuery(w, r, caller(r))
		if !ok {
			return
		}
		if err := s.store.DeleteQuery(q.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func caller(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey{}).(*principal)
	return p
}

func origin(r *http.Request) string {
	if r.Header.Get("X-Vigil-Origin") == originFiles {
		return originFiles
	}
	return originServer
}

// ownQuery finds the query a request names, if the caller may change it.
// Someone else's personal query does not exist as far as the caller can
// tell.
func (s *Server) ownQuery(w http.ResponseWriter, r *http.Request, p *principal) (SavedQuery, bool) {
	q, found, err := s.store.Query(r.PathValue("id"))
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case !found || (!q.Shared && q.Owner != p.User):
		http.Error(w, "no such saved query", http.StatusNotFound)
	case q.Shared && p.Role != RoleAdmin:
		http.Error(w, "a shared query is changed by an admin; save a copy of your own instead", http.StatusForbidden)
	default:
		return q, true
	}
	return q, false
}

// checkQuery says whether q may be saved as it is: it is well formed, the
// caller may share if it is shared, its name is free where it will live,
// and there is room for it.
func (s *Server) checkQuery(w http.ResponseWriter, p *principal, q *SavedQuery) bool {
	if err := q.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if q.Shared && p.Role != RoleAdmin {
		http.Error(w, "sharing a query with everyone needs an admin account", http.StatusForbidden)
		return false
	}
	clash, count, err := s.store.QueryRoom(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	where, limit := "You already have", maxOwnQueries
	if q.Shared {
		where, limit = "There already is", maxSharedQueries
	}
	if clash {
		http.Error(w, fmt.Sprintf("%s a saved %s query called %q.", where, q.Type, q.Name), http.StatusConflict)
		return false
	}
	if count >= limit {
		http.Error(w, fmt.Sprintf("that is the most saved queries there can be (%d); delete some first", limit), http.StatusConflict)
		return false
	}
	return true
}

func (s *Server) saveQuery(w http.ResponseWriter, q *SavedQuery) {
	now := time.Now().UTC().Truncate(time.Second)
	q.Updated, q.Editable = &now, true
	if err := s.store.PutQuery(q); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, q)
}

// Queries lists what a user sees: their own and the shared ones.
func (s *Store) Queries(user string) ([]SavedQuery, error) {
	rows, err := s.db.Query(`SELECT id, type, name, query, owner, shared, origin, updated FROM saved_queries
		WHERE shared = 1 OR owner = ? ORDER BY name COLLATE NOCASE`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedQuery{}
	for rows.Next() {
		var q SavedQuery
		var at int64
		if err := rows.Scan(&q.ID, &q.Type, &q.Name, &q.Query.Query, &q.Owner, &q.Shared, &q.Origin, &at); err != nil {
			return nil, err
		}
		t := time.Unix(at, 0).UTC()
		q.Updated = &t
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) Query(id string) (SavedQuery, bool, error) {
	q := SavedQuery{ID: id}
	var at int64
	err := s.db.QueryRow(`SELECT type, name, query, owner, shared, origin, updated FROM saved_queries WHERE id = ?`, id).
		Scan(&q.Type, &q.Name, &q.Query.Query, &q.Owner, &q.Shared, &q.Origin, &at)
	if err == sql.ErrNoRows {
		return q, false, nil
	}
	t := time.Unix(at, 0).UTC()
	q.Updated = &t
	return q, err == nil, err
}

// QueryRoom looks at where q is about to live, the shared list or its
// owner's own: whether another query there has its name and type, and how
// many others there are.
func (s *Store) QueryRoom(q *SavedQuery) (clash bool, count int, err error) {
	// The shared list is one place; the personal ones are a place per owner.
	owner := q.Owner
	if q.Shared {
		owner = ""
	}
	var same int
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(type = ? AND name = ? COLLATE NOCASE), 0) FROM saved_queries
		WHERE id != ? AND shared = ? AND (shared = 1 OR owner = ?)`, q.Type, q.Name, q.ID, q.Shared, owner).Scan(&count, &same)
	return same > 0, count, err
}

func (s *Store) PutQuery(q *SavedQuery) error {
	_, err := s.db.Exec(`INSERT INTO saved_queries (id, type, name, query, owner, shared, origin, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, query = excluded.query, owner = excluded.owner,
			shared = excluded.shared, origin = excluded.origin, updated = excluded.updated`,
		q.ID, q.Type, q.Name, q.Query.Query, q.Owner, q.Shared, q.Origin, q.Updated.Unix())
	return err
}

func (s *Store) DeleteQuery(id string) error {
	_, err := s.db.Exec(`DELETE FROM saved_queries WHERE id = ?`, id)
	return err
}
