package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/incidents"
)

// The briefing is what the Home page shows: the open incidents, what
// happened since the person last looked, and whether the picture is
// complete. Incidents are kept up to date in the background from the
// active alerts (internal/incidents); a request only reads that table and
// the agent list, never the stores.

const (
	incidentInterval  = 15 * time.Second
	incidentRetention = 30 * 24 * time.Hour
	recentWindow      = 24 * time.Hour
	// After a start, rule alerts come back only as their groups next run;
	// until then a missing alert resolves nothing.
	incidentWarmUp = 3 * time.Minute
	// An alert that fired this often in a week is listed as a tuning candidate.
	noisyFirings = 5
)

// incidentSync is how the last pass went, for the coverage part.
type incidentSync struct {
	At       time.Time `json:"at"`
	Error    string    `json:"error,omitempty"`
	Silences int       `json:"silences"`
	Alerts   int       `json:"alerts"`
}

// incidentLoop feeds the active alerts to the incident tracker.
func (s *Server) incidentLoop(ctx context.Context) {
	t := time.NewTicker(incidentInterval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		now := time.Now()
		s.syncIncidents(ctx, now)
		if now.Sub(lastPrune) > time.Hour {
			if err := s.incidents.Prune(ctx, now.Add(-incidentRetention)); err != nil && ctx.Err() == nil {
				s.log.Warn("pruning old incidents failed", "err", err)
			}
			lastPrune = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncIncidents is one pass: the active alerts, rule alerts and security
// detections alike, become or keep incidents.
func (s *Server) syncIncidents(ctx context.Context, now time.Time) {
	st := incidentSync{At: now}
	alerts, silences, err := s.am.Active(ctx)
	if err == nil {
		signals := make([]incidents.Signal, 0, len(alerts))
		for _, a := range alerts {
			signals = append(signals, incidents.FromAlert(a))
		}
		// Without the alert list nothing is resolved: "no data" is not
		// "all clear".
		err = s.incidents.Sync(ctx, signals, now, now.Sub(s.started) > incidentWarmUp)
		st.Silences, st.Alerts = silences, len(alerts)
	}
	if err != nil && ctx.Err() == nil {
		if prev := s.incidentState.Load(); prev == nil || prev.Error == "" {
			s.log.Warn("incidents are not being updated", "err", err)
		}
		st.Error = err.Error()
	}
	s.incidentState.Store(&st)
}

func (s *Server) briefingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/briefing", s.viewer(s.handleBriefing))
	mux.HandleFunc("POST /api/v1/briefing/seen", s.viewer(s.handleBriefingSeen))
	mux.HandleFunc("GET /api/v1/incidents", s.viewer(s.handleIncidents))
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.viewer(s.handleIncident))
	mux.HandleFunc("POST /api/v1/incidents/{id}/ack", s.admin(s.handleIncidentAck))
}

// coverage says how complete the briefing is: an empty incident list
// means little if half the agents are silent or rules cannot run.
type coverage struct {
	Agents       int           `json:"agents"`
	Reporting    int           `json:"reporting"`
	Silent       []string      `json:"silent"`
	RulesFailing []string      `json:"rules_failing"`
	Silences     int           `json:"silences"`
	Sync         *incidentSync `json:"sync"`
}

func (s *Server) coverage(now time.Time) (coverage, error) {
	c := coverage{Silent: []string{}, RulesFailing: []string{}, Sync: s.incidentState.Load()}
	agents, err := s.store.Agents(now, s.cfg.HeartbeatTimeout.Std())
	if err != nil {
		return c, err
	}
	for _, a := range agents {
		c.Agents++
		if a.Up {
			c.Reporting++
		} else {
			c.Silent = append(c.Silent, a.ID)
		}
	}
	for _, g := range s.rules.Status() {
		for _, r := range g.Rules {
			if r.Health == "err" {
				c.RulesFailing = append(c.RulesFailing, g.Name+"/"+r.Name)
			}
		}
	}
	if c.Sync != nil {
		c.Silences = c.Sync.Silences
	}
	return c, nil
}

func (s *Server) handleBriefing(w http.ResponseWriter, r *http.Request) {
	b, err := s.briefing(r.Context(), caller(r).User, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, b)
}

func (s *Server) briefing(ctx context.Context, user string, now time.Time) (map[string]any, error) {
	open, err := s.incidents.Open(ctx)
	if err != nil {
		return nil, err
	}
	recent, err := s.incidents.Resolved(ctx, now.Add(-recentWindow), 50)
	if err != nil {
		return nil, err
	}
	seen, err := s.incidents.Seen(ctx, user)
	if err != nil {
		return nil, err
	}
	since, err := s.incidents.Since(ctx, seen)
	if err != nil {
		return nil, err
	}
	cov, err := s.coverage(now)
	if err != nil {
		return nil, err
	}
	coming := s.comingState.Load()
	if coming == nil {
		coming = &comingState{Items: []Coming{}} // the first pass has not finished yet
	}
	noisy, err := s.incidents.Noisy(ctx, now.Add(-7*24*time.Hour), noisyFirings)
	if err != nil {
		return nil, err
	}
	return map[string]any{"user": user, "generated": now.UTC(), "open": open, "resolved": recent,
		"since": since, "coverage": cov, "coming": coming, "spend": s.spendState.Load(), "noisy": noisy}, nil
}

// handleBriefingSeen moves the caller's "seen up to" marker to the event
// id of the briefing they were shown.
func (s *Server) handleBriefingSeen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EventID int64 `json:"event_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.incidents.MarkSeen(r.Context(), caller(r).User, in.EventID, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIncidents lists resolved incidents, newest first, for history:
// ?days=7 (at most 30), ?limit=100 (at most 500).
func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	days, limit := 7, 100
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 {
		days = min(v, 30)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	list, err := s.incidents.Resolved(r.Context(), time.Now().Add(-time.Duration(days)*24*time.Hour), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func incidentID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not an incident id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func (s *Server) handleIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentID(w, r)
	if !ok {
		return
	}
	in, events, err := s.incidents.Get(r.Context(), id)
	switch {
	case errors.Is(err, incidents.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, map[string]any{"incident": in, "events": events})
	}
}

func (s *Server) handleIncidentAck(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentID(w, r)
	if !ok {
		return
	}
	var in struct {
		Comment string `json:"comment"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &in) {
		return
	}
	if len(in.Comment) > 500 {
		http.Error(w, "the comment is longer than 500 characters", http.StatusBadRequest)
		return
	}
	err := s.incidents.Acknowledge(r.Context(), id, caller(r).User, strings.TrimSpace(in.Comment), time.Now())
	switch {
	case errors.Is(err, incidents.ErrNotFound):
		http.Error(w, "no open incident with that id", http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
