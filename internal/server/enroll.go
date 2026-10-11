package server

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// Version is the server's build version, set by main.
var Version = "dev"

func (s *Server) enrollRoutes(mux *http.ServeMux) {
	// The authority's certificate is public by nature. An agent fetches it
	// before it can verify the server, and accepts it only if its
	// fingerprint is the one it was given.
	mux.HandleFunc("GET /api/v1/ca", func(w http.ResponseWriter, _ *http.Request) {
		if s.ca == nil {
			http.Error(w, "this server does not use its own certificate authority", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(s.ca.CAPEM())
	})
	// What an agent's file needs to say about this server.
	mux.HandleFunc("GET /api/v1/server-info", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		info := map[string]any{"url": strings.TrimRight(s.cfg.ExternalURL, "/"), "version": Version, "traces": s.traces != nil,
			"encrypted": s.ca != nil || s.cfg.TLS.CertFile != "" || strings.HasPrefix(s.cfg.ExternalURL, "https://")}
		if s.ca != nil {
			info["ca_pin"] = s.ca.Pin()
		}
		writeJSON(w, info)
	}))

	mux.HandleFunc("POST /api/v1/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/v1/fleet/enrollment", s.admin(func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.store.EnrollTokens()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("POST /api/v1/fleet/enrollment", s.admin(s.createEnrollToken))
	mux.HandleFunc("DELETE /api/v1/fleet/enrollment/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DeleteEnrollToken(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// createEnrollToken makes a token that hosts exchange for a token of their
// own. The token is returned once; the server keeps only its hash.
func (s *Server) createEnrollToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		ValidHours int    `json:"valid_hours"` // 0: does not expire
		MaxUses    int    `json:"max_uses"`    // 0: unlimited
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 || in.ValidHours < 0 || in.MaxUses < 0 {
		http.Error(w, "give the token a name; valid_hours and max_uses cannot be negative", http.StatusBadRequest)
		return
	}
	t := EnrollToken{ID: randomToken()[:12], Name: in.Name, MaxUses: in.MaxUses, Author: author(r)}
	if in.ValidHours > 0 {
		t.Expires = time.Now().Add(time.Duration(in.ValidHours) * time.Hour)
	}
	secret := "vge_" + randomToken()
	if err := s.store.CreateEnrollToken(t, hashToken(secret)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": t.ID, "name": t.Name, "token": secret, "expires": t.Expires})
}

// handleEnroll is what a new host calls: enrolment token and the name it
// wants in, a token of its own out. It needs no other credential, so
// guessing is slowed down like sign-in attempts.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token   string `json:"token"`
		AgentID string `json:"agent_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	addr := s.clientAddr(r)
	if s.logins.blocked(addr) {
		http.Error(w, "too many failed attempts, try again in a few minutes", http.StatusTooManyRequests)
		return
	}
	if !agentIDRe.MatchString(in.AgentID) || in.AgentID == selfAgentName {
		http.Error(w, "agent ids may use letters, digits and . _ -", http.StatusBadRequest)
		return
	}
	agentToken := "vgl_" + randomToken()
	err := s.store.Enroll(hashToken(in.Token), in.AgentID, hashToken(agentToken))
	var refused ErrEnroll
	switch {
	case errors.As(err, &refused):
		s.logins.fail(addr)
		s.log.Warn("enrolment refused", "agent", in.AgentID, "from", addr, "reason", refused)
		http.Error(w, string(refused), http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("agent enrolled", "agent", in.AgentID, "from", addr)
	writeJSON(w, map[string]string{"agent_id": in.AgentID, "token": agentToken})
}
