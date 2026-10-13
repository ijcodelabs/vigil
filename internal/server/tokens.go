package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Read-only tokens let a tool ask this server what a viewer may see:
// Grafana's data sources, a script that pulls numbers. They go in the
// Authorization header as "Bearer vgr_…". Such a caller is a viewer that
// cannot change anything: every request other than GET is refused, except
// on the query APIs, which take their questions by POST too.
//
// The admin token is the only other credential a tool could use, and it
// can do everything; that is why these exist.

const readTokenPrefix = "vgr_"

// APIToken is a read-only token as the list shows it. The token itself is
// shown once, when it is made.
type APIToken struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Author   string     `json:"author"`
	Created  time.Time  `json:"created"`
	Expires  *time.Time `json:"expires,omitempty"`   // absent: does not expire
	LastUsed *time.Time `json:"last_used,omitempty"` // to the minute; absent: never
}

// tokenUse remembers when each token's use was last written down, so that
// a dashboard's hundred queries are one write a minute and not a hundred.
var tokenUse sync.Map // id -> unix time

// readToken looks a token up and returns the name it was given.
func (s *Server) readToken(token string) (string, bool) {
	id, name, expires, err := s.store.ReadToken(hashToken(token))
	now := time.Now().Unix()
	if err != nil || (expires != 0 && expires < now) {
		return "", false
	}
	if last, _ := tokenUse.Load(id); last == nil || now-last.(int64) >= 60 {
		tokenUse.Store(id, now)
		if err := s.store.TouchToken(id, now); err != nil {
			s.log.Warn("noting the use of a token failed", "token", name, "err", err)
		}
	}
	return name, true
}

func (s *Server) tokenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tokens", s.admin(func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.store.APITokens()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("POST /api/v1/tokens", s.admin(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string `json:"name"`
			ValidDays int    `json:"valid_days"` // 0: does not expire
		}
		if !readJSON(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 100 || in.ValidDays < 0 || in.ValidDays > 3650 {
			http.Error(w, "give the token a name; valid_days is 0 (no end) to 3650", http.StatusBadRequest)
			return
		}
		t := APIToken{ID: randomToken()[:12], Name: in.Name, Author: author(r), Created: time.Now().UTC().Truncate(time.Second)}
		if in.ValidDays > 0 {
			exp := t.Created.AddDate(0, 0, in.ValidDays)
			t.Expires = &exp
		}
		secret := readTokenPrefix + randomToken()
		if err := s.store.CreateAPIToken(t, hashToken(secret)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"id": t.ID, "name": t.Name, "token": secret, "expires": t.Expires})
	}))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DeleteAPIToken(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tokenUse.Delete(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Store) CreateAPIToken(t APIToken, tokenHash string) error {
	exp := int64(0)
	if t.Expires != nil {
		exp = t.Expires.Unix()
	}
	_, err := s.db.Exec(`INSERT INTO api_tokens (id, name, token_hash, expires, author, created) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, tokenHash, exp, t.Author, t.Created.Unix())
	return err
}

func (s *Store) APITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, expires, author, created, last_used FROM api_tokens ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var exp, created, used int64
		if err := rows.Scan(&t.ID, &t.Name, &exp, &t.Author, &created, &used); err != nil {
			return nil, err
		}
		t.Created = time.Unix(created, 0).UTC()
		if exp != 0 {
			e := time.Unix(exp, 0).UTC()
			t.Expires = &e
		}
		if used != 0 {
			u := time.Unix(used, 0).UTC()
			t.LastUsed = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReadToken finds a token by its hash. A token that does not exist is an
// error like any other: the caller is not let in.
func (s *Store) ReadToken(tokenHash string) (id, name string, expires int64, err error) {
	err = s.db.QueryRow(`SELECT id, name, expires FROM api_tokens WHERE token_hash = ?`, tokenHash).Scan(&id, &name, &expires)
	return id, name, expires, err
}

func (s *Store) TouchToken(id string, at int64) error {
	_, err := s.db.Exec(`UPDATE api_tokens SET last_used = ? WHERE id = ?`, at, id)
	return err
}

func (s *Store) DeleteAPIToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	return err
}
