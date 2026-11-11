package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/pki"
)

// Identities are the names the server's certificate authority signs
// certificates for, besides its own (memory/context/identity.md):
//
//   - a client, such as Portun Chat sending its events to an agent's OTLP
//     endpoint: a certificate naming vigil://client/<name>?epoch=<n>, signed
//     for the agents an admin allowed to hold it, which renew it for the
//     program on their host;
//   - an endpoint: a serving certificate for an agent's OTLP endpoint, only
//     for the host names and addresses an admin approved. The same
//     authority signs the server's own certificate, so an agent must not be
//     able to get one for any name it likes.
//
// Revoking a name bumps its epoch: endpoints accept only the current epoch,
// so every certificate ever signed for that name stops working, also a copy
// someone took.

const (
	identityClient   = "client"
	identityEndpoint = "endpoint"
)

// Identity is one approved name.
type Identity struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`            // client or endpoint
	Agents  string    `json:"agents"`          // regular expression: the agents that may hold it
	Names   []string  `json:"names,omitempty"` // endpoint: its host names and addresses
	Epoch   int       `json:"epoch"`
	Revoked bool      `json:"revoked"`
	Author  string    `json:"author"`
	Updated time.Time `json:"updated"`
	// LastIssued is when a certificate was last signed for it.
	LastIssued *time.Time `json:"last_issued,omitempty"`
}

func initIdentities(db *sql.DB) error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS identities (name TEXT PRIMARY KEY, kind TEXT NOT NULL, agents TEXT NOT NULL,
			names TEXT NOT NULL DEFAULT '', epoch INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0,
			author TEXT NOT NULL, updated INTEGER NOT NULL)`,
		// Every certificate signed, for the record: what, for whom, until when.
		`CREATE TABLE IF NOT EXISTS identity_certs (serial TEXT PRIMARY KEY, name TEXT NOT NULL, epoch INTEGER NOT NULL,
			agent TEXT NOT NULL, not_after INTEGER NOT NULL, issued INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) identityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/identities", s.admin(s.handleListIdentities))
	mux.HandleFunc("PUT /api/v1/identities/{name}", s.admin(s.handlePutIdentity))
	mux.HandleFunc("POST /api/v1/identities/{name}/revoke", s.admin(s.handleRevokeIdentity))
	mux.HandleFunc("DELETE /api/v1/identities/{name}", s.admin(s.handleDeleteIdentity))
	// Agents, with their own token.
	mux.HandleFunc("POST /api/v1/identity/sign", s.handleIdentitySign)
	mux.HandleFunc("GET /api/v1/identity/state", s.handleIdentityState)
}

func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	list, err := s.identities("")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handlePutIdentity(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		Kind   string   `json:"kind"`
		Agents string   `json:"agents"`
		Names  []string `json:"names"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Kind == "" {
		in.Kind = identityClient
	}
	switch {
	case !pki.NameRe.MatchString(name):
		http.Error(w, "a name is lower-case letters, digits and ._-", http.StatusBadRequest)
		return
	case in.Kind != identityClient && in.Kind != identityEndpoint:
		http.Error(w, "kind is client or endpoint", http.StatusBadRequest)
		return
	case in.Agents == "" || len(in.Agents) > 300:
		http.Error(w, "agents is a regular expression of the agent ids that may hold it", http.StatusBadRequest)
		return
	case (in.Kind == identityEndpoint) != (len(in.Names) > 0) || len(in.Names) > 16:
		http.Error(w, "an endpoint names its host names or addresses (1 to 16); a client names none", http.StatusBadRequest)
		return
	}
	if _, err := regexp.Compile("^(?:" + in.Agents + ")$"); err != nil {
		http.Error(w, "agents: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, n := range in.Names {
		if strings.ContainsAny(n, ", ") || n == "" {
			http.Error(w, "names: one host name or address each", http.StatusBadRequest)
			return
		}
	}
	// A new name starts at epoch 1; approving a revoked one starts a new
	// epoch, so what was signed before it was revoked stays dead.
	now := time.Now().Unix()
	_, err := s.store.db.Exec(`INSERT INTO identities (name, kind, agents, names, epoch, revoked, author, updated) VALUES (?, ?, ?, ?, 1, 0, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, agents = excluded.agents, names = excluded.names,
			epoch = CASE WHEN identities.revoked = 1 THEN identities.epoch + 1 ELSE identities.epoch END,
			revoked = 0, author = excluded.author, updated = excluded.updated`,
		name, in.Kind, in.Agents, strings.Join(in.Names, ","), author(r), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("identity approved", "name", name, "kind", in.Kind, "agents", in.Agents, "by", author(r))
	s.writeIdentity(w, name)
}

func (s *Server) handleRevokeIdentity(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.store.db.Exec(`UPDATE identities SET epoch = epoch + 1, revoked = 1, author = ?, updated = ? WHERE name = ?`,
		author(r), time.Now().Unix(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.NotFound(w, r)
		return
	}
	s.log.Info("identity revoked", "name", name, "by", author(r))
	s.writeIdentity(w, name)
}

func (s *Server) handleDeleteIdentity(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.db.Exec(`DELETE FROM identities WHERE name = ?`, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("identity removed", "name", r.PathValue("name"), "by", author(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeIdentity(w http.ResponseWriter, name string) {
	list, err := s.identities(name)
	if err != nil || len(list) == 0 {
		http.Error(w, "identity not found", http.StatusNotFound)
		return
	}
	writeJSON(w, list[0])
}

func (s *Server) identities(name string) ([]Identity, error) {
	q := `SELECT i.name, i.kind, i.agents, i.names, i.epoch, i.revoked, i.author, i.updated,
		(SELECT MAX(issued) FROM identity_certs c WHERE c.name = i.name) FROM identities i`
	args := []any{}
	if name != "" {
		q += ` WHERE i.name = ?`
		args = append(args, name)
	}
	rows, err := s.store.db.Query(q+` ORDER BY i.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var (
			id      Identity
			names   string
			updated int64
			issued  sql.NullInt64
		)
		if err := rows.Scan(&id.Name, &id.Kind, &id.Agents, &names, &id.Epoch, &id.Revoked, &id.Author, &updated, &issued); err != nil {
			return nil, err
		}
		if names != "" {
			id.Names = strings.Split(names, ",")
		}
		id.Updated = time.Unix(updated, 0).UTC()
		if issued.Valid {
			t := time.Unix(issued.Int64, 0).UTC()
			id.LastIssued = &t
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ownAgent lets in only an agent with a token of its own, for its own id:
// the shared ingest token says nothing about which agent is asking.
func (s *Server) ownAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := agentID(w, r)
	if !ok {
		return "", false
	}
	tok := bearer(r)
	if tok == "" || sameToken(tok, s.cfg.IngestToken) || s.tokenOwner(hashToken(tok)) != id {
		http.Error(w, "this needs the agent's own token (enrol it)", http.StatusUnauthorized)
		return "", false
	}
	return id, true
}

var errIdentityNotAllowed = errors.New("this agent may not hold that name")

func (s *Server) allowedIdentity(name, agent string) (Identity, error) {
	list, err := s.identities(name)
	if err != nil {
		return Identity{}, err
	}
	if len(list) == 0 || list[0].Revoked {
		return Identity{}, errIdentityNotAllowed
	}
	re, err := regexp.Compile("^(?:" + list[0].Agents + ")$")
	if err != nil || !re.MatchString(agent) {
		return Identity{}, errIdentityNotAllowed
	}
	return list[0], nil
}

func (s *Server) handleIdentitySign(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.ownAgent(w, r)
	if !ok {
		return
	}
	if s.ca == nil {
		http.Error(w, "this server has no certificate authority (tls.self_signed)", http.StatusNotImplemented)
		return
	}
	var in struct {
		Name string `json:"name"`
		CSR  string `json:"csr"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.allowedIdentity(in.Name, agent)
	if err != nil {
		s.log.Warn("certificate refused", "name", in.Name, "agent", agent, "err", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	now := time.Now()
	var certPEM []byte
	if id.Kind == identityEndpoint {
		certPEM, err = s.ca.SignServing([]byte(in.CSR), id.Names, now)
	} else {
		certPEM, err = s.ca.SignClient([]byte(in.CSR), id.Name, id.Epoch, now)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	serial, notAfter := pki.SerialAndExpiry(certPEM)
	if _, err := s.store.db.Exec(`INSERT INTO identity_certs (serial, name, epoch, agent, not_after, issued) VALUES (?, ?, ?, ?, ?, ?)`,
		serial, id.Name, id.Epoch, agent, notAfter.Unix(), now.Unix()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("certificate signed", "name", id.Name, "kind", id.Kind, "epoch", id.Epoch, "agent", agent, "until", notAfter)
	writeJSON(w, map[string]any{"cert": string(certPEM), "ca": string(s.ca.CAPEM()), "kind": id.Kind, "epoch": id.Epoch,
		"not_after": notAfter})
}

// handleIdentityState tells an agent's OTLP endpoint which clients are
// current, at which epoch; a name that is not listed is not let in.
func (s *Server) handleIdentityState(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.ownAgent(w, r)
	if !ok {
		return
	}
	list, err := s.identities("")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Endpoints: the approved names of those this agent may hold, so it
	// renews a certificate whose names an admin changed.
	epochs, endpoints := map[string]int{}, map[string][]string{}
	for _, id := range list {
		switch {
		case id.Revoked:
		case id.Kind == identityClient:
			epochs[id.Name] = id.Epoch
		case id.Kind == identityEndpoint:
			if re, err := regexp.Compile("^(?:" + id.Agents + ")$"); err == nil && re.MatchString(agent) {
				endpoints[id.Name] = id.Names
			}
		}
	}
	ca := ""
	if s.ca != nil {
		ca = string(s.ca.CAPEM())
	}
	writeJSON(w, map[string]any{"epochs": epochs, "endpoints": endpoints, "ca": ca})
}
