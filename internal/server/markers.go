package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Markers are things that happened to the work, drawn on charts: what Portun
// did (items opened and moved, investigations, workroom runs; from its
// audit events, observability contract v1) and vigil's own configuration
// changes. A chart asks for the markers of one service or one work item
// over its range; only events that name that service or that work item
// count, nothing is joined or guessed. Portun is the record of what happened:
// these are its events as received.

const (
	markersMax      = 500
	markersTTL      = 20 * time.Second
	markersMaxRange = 31 * 24 * time.Hour
)

// The Portun events that make markers.
var markerEvents = []string{"portun.item.opened", "portun.item.moved", "portun.investigation.started", "portun.investigation.proposed",
	"portun.investigation.accepted", "portun.investigation.dismissed", "portun.investigation.failed", "portun.task.started", "portun.task.ended"}

var (
	markerServiceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	markerItemRe    = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,15}-[0-9]{1,9}$`)
)

// Marker is one thing to draw at a time.
type Marker struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"` // item, investigation, task, config
	Event    string    `json:"event,omitempty"`
	Title    string    `json:"title"`
	WorkItem string    `json:"work_item,omitempty"`
	Service  string    `json:"service,omitempty"`
}

type markerCache struct {
	mu sync.Mutex
	m  map[string]markerEntry
}

type markerEntry struct {
	at   time.Time
	list []Marker
	more bool
}

func (s *Server) markerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/markers", s.viewer(s.handleMarkers))
}

func (s *Server) handleMarkers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service, item, incident := q.Get("service"), q.Get("work_item"), q.Get("incident")
	given := 0
	for _, v := range []string{service, item, incident} {
		if v != "" {
			given++
		}
	}
	switch {
	case given != 1:
		http.Error(w, "give one of service, work_item or incident", http.StatusBadRequest)
		return
	case incident != "":
		id, err := strconv.ParseInt(incident, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "incident is vigil's incident id", http.StatusBadRequest)
			return
		}
	case service != "" && !markerServiceRe.MatchString(service):
		http.Error(w, "service: lower-case letters, digits and ._- (as Portun sends it)", http.StatusBadRequest)
		return
	case item != "" && !markerItemRe.MatchString(item):
		http.Error(w, "work_item is a key like OPS-12", http.StatusBadRequest)
		return
	}
	now := time.Now()
	from, to := now.Add(-time.Hour), now
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				http.Error(w, name+" is Unix seconds", http.StatusBadRequest)
				return
			}
			*dst = time.Unix(int64(f), 0)
		}
	}
	if !from.Before(to) || to.Sub(from) > markersMaxRange {
		http.Error(w, "from must be before to, at most 31 days apart", http.StatusBadRequest)
		return
	}
	if incident != "" {
		// The Portun item an investigation of this incident belongs to: the
		// only exact link there is today.
		key, err := s.itemOfIncident(r.Context(), incident, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if key == "" {
			writeJSON(w, map[string]any{"markers": []Marker{}, "more": false})
			return
		}
		item = key
	}
	// Ranges that end at "now" move every second; a cache key to the 20 s
	// makes the charts of one page and their refreshes share one query.
	key := fmt.Sprintf("%s|%s|%d|%d", service, item, from.Unix()/20, to.Unix()/20)
	s.markers.mu.Lock()
	e, ok := s.markers.m[key]
	s.markers.mu.Unlock()
	if !ok || now.Sub(e.at) > markersTTL {
		list, more, err := s.loadMarkers(r.Context(), service, item, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		e = markerEntry{at: now, list: list, more: more}
		s.markers.mu.Lock()
		if s.markers.m == nil || len(s.markers.m) > 1000 {
			s.markers.m = map[string]markerEntry{}
		}
		s.markers.m[key] = e
		s.markers.mu.Unlock()
	}
	writeJSON(w, map[string]any{"markers": e.list, "more": e.more, "work_item": item})
}

// itemOfIncident finds the work item whose investigation read this
// incident's evidence ("" when there is none).
func (s *Server) itemOfIncident(ctx context.Context, incident string, from, to time.Time) (string, error) {
	rows, err := s.logQuery(ctx, `logger:="portun.audit" event_name:="portun.investigation.started" vigil_incident:=`+strconv.Quote(incident)+
		` | sort by (_time desc) | limit 1`, from.Add(-7*24*time.Hour), to, 1)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	if key := rows[0]["work_item_key"]; markerItemRe.MatchString(key) {
		return key, nil
	}
	return "", nil
}

func (s *Server) loadMarkers(ctx context.Context, service, item string, from, to time.Time) ([]Marker, bool, error) {
	names := make([]string, len(markerEvents))
	for i, n := range markerEvents {
		names[i] = strconv.Quote(n)
	}
	filter := "work_item_key:=" + strconv.Quote(item)
	if service != "" {
		filter = "item_service:=" + strconv.Quote(service)
	}
	query := fmt.Sprintf(`logger:="portun.audit" event_name:in(%s) %s | sort by (_time desc) | limit %d`,
		strings.Join(names, ","), filter, markersMax+1)
	rows, err := s.logQuery(ctx, query, from, to, markersMax+1)
	if err != nil {
		return nil, false, err
	}
	out := make([]Marker, 0, len(rows))
	for _, row := range rows {
		t, err := time.Parse(time.RFC3339Nano, row["_time"])
		if err != nil {
			continue
		}
		out = append(out, markerOf(t, row))
	}
	more := len(out) > markersMax
	if more {
		out = out[:markersMax]
	}
	if service != "" { // vigil's own changes are everyone's
		out = append(out, s.configMarkers(ctx, from, to)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, more, nil
}

// markerOf makes a marker of one Portun event, from its bounded fields only.
func markerOf(t time.Time, f map[string]string) Marker {
	key, ev := f["work_item_key"], f["event_name"]
	m := Marker{Time: t, Event: ev, WorkItem: key, Service: f["item_service"]}
	item := key
	if item == "" {
		item = "an item"
	}
	switch ev {
	case "portun.item.opened":
		m.Kind = "item"
		m.Title = fmt.Sprintf("%s opened (%s%s)", item, orDash(f["item_type"]), sevSuffix(f["severity"]))
	case "portun.item.moved":
		m.Kind = "item"
		m.Title = fmt.Sprintf("%s → %s (from %s)", item, orDash(f["to_category"]), orDash(f["from_category"]))
	case "portun.task.started":
		m.Kind = "task"
		m.Title = "Workroom run started"
		if f["mode"] == "investigate" {
			m.Title = "Investigation run started"
		}
		if key != "" {
			m.Title += " for " + key
		}
	case "portun.task.ended":
		m.Kind = "task"
		m.Title = "Workroom run ended: " + orDash(f["reason"])
		if key != "" {
			m.Title += " (" + key + ")"
		}
	default: // portun.investigation.*
		m.Kind = "investigation"
		m.Title = fmt.Sprintf("Investigation of %s %s", item, strings.TrimPrefix(ev, "portun.investigation."))
	}
	return m
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func sevSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ", " + s
}

// configMarkers are the profile versions saved or deleted in the range.
func (s *Server) configMarkers(ctx context.Context, from, to time.Time) []Marker {
	rows, err := s.store.db.QueryContext(ctx, `SELECT name, version, time, deleted FROM profile_versions
		WHERE time BETWEEN ? AND ? ORDER BY time LIMIT 100`, from.Unix(), to.Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Marker
	for rows.Next() {
		var (
			name        string
			version, ts int64
			deleted     bool
		)
		if rows.Scan(&name, &version, &ts, &deleted) == nil {
			what := "saved"
			if deleted {
				what = "deleted"
			}
			out = append(out, Marker{Time: time.Unix(ts, 0).UTC(), Kind: "config", Title: fmt.Sprintf("Profile %s v%d %s", name, version, what)})
		}
	}
	return out
}
