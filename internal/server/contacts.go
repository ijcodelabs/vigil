package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
)

// Contacts & routing can be tried out before an alert depends on them: a
// test message through one integration of a contact, and which contacts an
// alert with given labels would reach. Both take an optional "config", the
// text of the editor before it is saved; without it they use what is saved.
// Admins only, like the configuration itself.

func (s *Server) contactRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/alerting/test", s.admin(s.handleAlertingTest))
	mux.HandleFunc("POST /api/v1/alerting/route", s.admin(s.handleAlertingRoute))
}

// readContactBody reads a JSON body that may carry a whole configuration.
func readContactBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody+64<<10)).Decode(v); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleAlertingTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		alerting.Target
		Config string `json:"config"`
	}
	if !readContactBody(w, r, &in) {
		return
	}
	if in.Config != "" {
		if err := alerting.Validate(in.Config); err != nil {
			http.Error(w, "invalid alerting config: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	by := ""
	if p, _ := r.Context().Value(principalKey{}).(*principal); p != nil {
		by = p.User
	}
	start := time.Now()
	err := s.am.SendTest(r.Context(), in.Config, in.Target, by)
	switch {
	case errors.Is(err, alerting.ErrNoIntegration):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, alerting.ErrTooSoon):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	// The integration's answer is the result, whichever it was.
	out := map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds()}
	if err != nil {
		out["error"] = err.Error()
	}
	s.log.Info("test notification", "receiver", in.Receiver, "integration", in.Integration, "index", in.Index,
		"by", by, "unsaved", in.Config != "", "ok", err == nil)
	writeJSON(w, out)
}

func (s *Server) handleAlertingRoute(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Labels map[string]string `json:"labels"`
		Config string            `json:"config"`
	}
	if !readContactBody(w, r, &in) {
		return
	}
	if len(in.Labels) == 0 {
		http.Error(w, "labels: give at least one, such as alertname", http.StatusBadRequest)
		return
	}
	res, err := s.am.Route(r.Context(), in.Config, in.Labels)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, res)
}
