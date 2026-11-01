package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/incidents"
)

// An evidence grant lets something that investigates one incident, such as
// an AI agent's run in a Boop workroom, read that incident's evidence
// bundle and nothing else: no queries, no other incident, no change. It is
// short-lived, used a bounded number of times, and every issue and use is
// logged. The bundle is the one Explain sends, built by vigil, masked
// again on the way out and limited to the data classes the server allows
// for AI (agentic.md, principle 4).
//
// Grants are issued by an admin or by an issuer token: a token whose only
// power is to issue grants, so the system that starts runs (Boop Chat)
// holds no token that can read anything itself.

const (
	evidenceTokenPrefix = "vge_"
	grantTTLDefault     = 30 * time.Minute
	grantTTLMax         = time.Hour
	grantUsesMax        = 20
	// A grant may name an incident resolved this long ago at most: the
	// investigation of something long over is not a run's business.
	grantLookback = 24 * time.Hour
)

// EvidenceGrant is a grant as the admin list shows it. The token is shown
// once, when it is issued.
type EvidenceGrant struct {
	ID       string     `json:"id"`
	Incident int64      `json:"incident"`
	Purpose  string     `json:"purpose"`
	Issuer   string     `json:"issuer"`
	Created  time.Time  `json:"created"`
	Expires  time.Time  `json:"expires"`
	Uses     int        `json:"uses"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func (s *Server) evidenceGrantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/evidence-grants", s.issuer(s.handleIssueGrant))
	mux.HandleFunc("GET /api/v1/evidence-grants", s.admin(s.handleListGrants))
	mux.HandleFunc("DELETE /api/v1/evidence-grants/{id}", s.admin(s.handleRevokeGrant))
	mux.HandleFunc("GET /api/v1/evidence", s.handleGrantEvidence)
}

// issuer lets in an admin or an issuer token, and nobody else.
func (s *Server) issuer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, viaCookie := s.identify(r)
		switch {
		case p == nil:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		case !p.Issuer && p.Role != RoleAdmin:
			http.Error(w, "this needs an admin account or an issuer token", http.StatusForbidden)
			return
		case viaCookie && r.Header.Get(csrfHeader) == "":
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func (s *Server) handleIssueGrant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		// Either the incident's id, or the labels of one of its alerts as
		// Alertmanager's webhook delivered them.
		Incident   int64             `json:"incident"`
		Labels     map[string]string `json:"labels"`
		TTLMinutes int               `json:"ttl_minutes"`
		Purpose    string            `json:"purpose"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Purpose = strings.TrimSpace(in.Purpose)
	ttl := time.Duration(in.TTLMinutes) * time.Minute
	if in.TTLMinutes == 0 {
		ttl = grantTTLDefault
	}
	switch {
	case (in.Incident == 0) == (len(in.Labels) == 0):
		http.Error(w, "give either incident or labels", http.StatusBadRequest)
		return
	case in.Purpose == "" || len(in.Purpose) > 120:
		http.Error(w, "say what the grant is for (purpose, at most 120 characters)", http.StatusBadRequest)
		return
	case ttl < time.Minute || ttl > grantTTLMax:
		http.Error(w, "ttl_minutes is 1 to 60", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var in2 incidents.Incident
	var err error
	if in.Incident != 0 {
		in2, _, err = s.incidents.Get(ctx, in.Incident)
	} else {
		in2, err = s.incidents.ByKey(ctx, incidents.FromAlert(alerting.ActiveAlert{Alert: alerting.Alert{Labels: in.Labels}}).Key)
	}
	now := time.Now().UTC().Truncate(time.Second)
	switch {
	case errors.Is(err, incidents.ErrNotFound):
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	case in2.State == incidents.StateResolved && now.Sub(in2.ResolvedAt) > grantLookback:
		http.Error(w, "the incident was resolved more than a day ago", http.StatusConflict)
		return
	}
	g := EvidenceGrant{ID: randomToken()[:12], Incident: in2.ID, Purpose: in.Purpose, Issuer: caller(r).User,
		Created: now, Expires: now.Add(ttl)}
	secret := evidenceTokenPrefix + randomToken()
	if err := s.store.CreateEvidenceGrant(g, hashToken(secret)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("evidence grant issued", "grant", g.ID, "incident", g.Incident, "by", g.Issuer, "purpose", g.Purpose, "expires", g.Expires)
	writeJSON(w, map[string]any{"id": g.ID, "incident": g.Incident, "token": secret, "expires": g.Expires})
}

func (s *Server) handleListGrants(w http.ResponseWriter, _ *http.Request) {
	list, err := s.store.EvidenceGrants(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteEvidenceGrant(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("evidence grant revoked", "grant", r.PathValue("id"), "by", caller(r).User)
	w.WriteHeader(http.StatusNoContent)
}

// handleGrantEvidence answers a grant's token with its incident's bundle.
// The token is the whole credential; nothing else is accepted here.
func (s *Server) handleGrantEvidence(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if !strings.HasPrefix(tok, evidenceTokenPrefix) {
		http.Error(w, "this needs an evidence grant's token", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	g, err := s.store.UseEvidenceGrant(hashToken(tok), now, grantUsesMax)
	switch {
	case errors.Is(err, errGrantSpent):
		http.Error(w, "this grant has been used up", http.StatusTooManyRequests)
		return
	case err != nil || g.Expires.Before(now):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	in, events, err := s.incidents.Get(ctx, g.Incident)
	if err != nil {
		http.Error(w, "the incident is gone", http.StatusNotFound)
		return
	}
	b := s.buildBundle(ctx, in, events)
	s.log.Info("evidence read", "grant", g.ID, "incident", g.Incident, "use", g.Uses, "bytes", b.Bytes)
	writeJSON(w, map[string]any{
		"incident": map[string]any{"id": in.ID, "title": clean(in.Title, 300), "kind": in.Kind, "resource": clean(in.Resource, 300),
			"severity": in.Severity, "state": in.State, "first_seen": in.FirstSeen, "last_seen": in.LastSeen, "resolved_at": in.ResolvedAt},
		"bundle":  b,
		"expires": g.Expires,
	})
}

var errGrantSpent = errors.New("grant used up")

func (s *Store) CreateEvidenceGrant(g EvidenceGrant, tokenHash string) error {
	_, err := s.db.Exec(`INSERT INTO evidence_grants (id, token_hash, incident, purpose, issuer, created, expires) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.ID, tokenHash, g.Incident, g.Purpose, g.Issuer, g.Created.Unix(), g.Expires.Unix())
	return err
}

// UseEvidenceGrant counts one use of a grant and returns it, or
// errGrantSpent once it has been used max times.
func (s *Store) UseEvidenceGrant(tokenHash string, now time.Time, max int) (EvidenceGrant, error) {
	res, err := s.db.Exec(`UPDATE evidence_grants SET uses = uses + 1, last_used = ? WHERE token_hash = ? AND uses < ?`,
		now.Unix(), tokenHash, max)
	if err != nil {
		return EvidenceGrant{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var uses int
		if err := s.db.QueryRow(`SELECT uses FROM evidence_grants WHERE token_hash = ?`, tokenHash).Scan(&uses); err != nil {
			return EvidenceGrant{}, err
		}
		return EvidenceGrant{}, errGrantSpent
	}
	list, err := s.grants(`token_hash = ?`, tokenHash)
	if err == nil && len(list) == 0 {
		err = errors.New("grant vanished")
	}
	if err != nil {
		return EvidenceGrant{}, err
	}
	return list[0], nil
}

// EvidenceGrants lists the grants that expired after since (or have not),
// newest first, and forgets older ones.
func (s *Store) EvidenceGrants(since time.Time) ([]EvidenceGrant, error) {
	if _, err := s.db.Exec(`DELETE FROM evidence_grants WHERE expires < ?`, since.Unix()); err != nil {
		return nil, err
	}
	return s.grants(`1 = 1 ORDER BY created DESC LIMIT 500`)
}

func (s *Store) DeleteEvidenceGrant(id string) error {
	_, err := s.db.Exec(`DELETE FROM evidence_grants WHERE id = ?`, id)
	return err
}

func (s *Store) grants(where string, args ...any) ([]EvidenceGrant, error) {
	rows, err := s.db.Query(`SELECT id, incident, purpose, issuer, created, expires, uses, last_used FROM evidence_grants WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvidenceGrant{}
	for rows.Next() {
		var g EvidenceGrant
		var created, expires, used int64
		if err := rows.Scan(&g.ID, &g.Incident, &g.Purpose, &g.Issuer, &created, &expires, &g.Uses, &used); err != nil {
			return nil, err
		}
		g.Created, g.Expires = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		if used != 0 {
			u := time.Unix(used, 0).UTC()
			g.LastUsed = &u
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
