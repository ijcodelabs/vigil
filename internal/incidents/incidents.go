// Package incidents turns what vigil knows is wrong into incidents: one
// per thing that is wrong, with a lifecycle (open, acknowledged, resolved,
// reopened) and an ordered history. The server feeds it the active alerts
// every few seconds (rule alerts and security detections both end up
// there); the Home page reads the result instead of querying the stores.
//
// Correlation is by containment only: while a host is down, its other
// incidents hang under the host's incident instead of standing beside it.
package incidents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
)

// Kinds of incident.
const (
	KindHostDown = "host_down" // the agent stopped reporting; the parent of that host's other incidents
	KindAlert    = "alert"
	KindSecurity = "security" // a runtime detection
	KindSLO      = "slo"      // an SLO spends its error budget too fast, or has spent it
)

// States of an incident.
const (
	StateOpen         = "open"
	StateAcknowledged = "acknowledged"
	StateResolved     = "resolved"
)

// Signal is one thing that says something is wrong right now.
type Signal struct {
	Key      string // kind, resource and what tells it apart; stable while the problem lasts
	Kind     string
	Resource string // slo:<name>, host:<agent>, container:<agent>/<name>, check:<agent>/<probe>, pod:<ns>/<pod>, backend:<agent>/<proxy>[/<server>], service:<name>, or empty
	// ParentResource is what contains Resource besides its host: an open
	// incident on it holds this one (a HAProxy server under its backend).
	ParentResource string
	Agent          string
	Title          string
	Severity       string
	Silenced       bool
	Since          time.Time
	Labels         map[string]string
	Annotations    map[string]string
}

// FromAlert makes a signal of an active alert.
func FromAlert(a alerting.ActiveAlert) Signal {
	l := a.Labels
	sig := Signal{Kind: KindAlert, Agent: l["agent"], Severity: l["severity"], Silenced: a.Silenced,
		Since: a.StartsAt, Labels: l, Annotations: a.Annotations}
	switch {
	case l["alertname"] == "HostDown":
		sig.Kind = KindHostDown
	case l["source"] == "security":
		sig.Kind = KindSecurity
	case l["slo"] != "" && strings.HasPrefix(l["alertname"], "SLO"):
		sig.Kind = KindSLO
	}
	switch {
	case sig.Kind == KindSLO:
		// The SLO is what is wrong, not a host: its alerts carry no agent.
		sig.Resource = "slo:" + l["slo"]
	case l["container"] != "" && l["agent"] != "":
		sig.Resource = "container:" + l["agent"] + "/" + l["container"]
	case l["probe"] != "":
		sig.Resource = "check:" + l["agent"] + "/" + l["probe"]
	case l["pod"] != "":
		sig.Resource = "pod:" + l["namespace"] + "/" + l["pod"]
	case l["proxy"] != "" && l["agent"] != "": // HAProxy: a backend, or one server of it
		sig.Resource = "backend:" + l["agent"] + "/" + l["proxy"]
		if l["server"] != "" {
			sig.ParentResource, sig.Resource = sig.Resource, sig.Resource+"/"+l["server"]
		}
	case l["agent"] != "":
		sig.Resource = "host:" + l["agent"]
	case l["service"] != "":
		sig.Resource = "service:" + l["service"]
	}
	sig.Title = a.Annotations["summary"]
	if sig.Title == "" {
		_, what, _ := strings.Cut(sig.Resource, ":")
		sig.Title = strings.TrimSpace(l["alertname"] + " " + what)
	}
	// Every label counts for identity, so two alerts that differ in any
	// label are two incidents even on the same resource.
	sig.Key = sig.Kind + "|" + sig.Resource + "|" + l["alertname"] + "|" + labelHash(l)
	return sig
}

func labelHash(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k + "\xff" + l[k] + "\xff"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Incident is one thing that is or was wrong.
type Incident struct {
	ID          int64             `json:"id"`
	Key         string            `json:"key"`
	Kind        string            `json:"kind"`
	Resource    string            `json:"resource"`
	ParentRes   string            `json:"parent_resource,omitempty"`
	Agent       string            `json:"agent,omitempty"`
	Title       string            `json:"title"`
	Severity    string            `json:"severity,omitempty"`
	State       string            `json:"state"`
	Silenced    bool              `json:"silenced"`
	Parent      int64             `json:"parent,omitempty"`
	FirstSeen   time.Time         `json:"first_seen"`
	LastSeen    time.Time         `json:"last_seen"`
	ResolvedAt  time.Time         `json:"resolved_at,omitzero"`
	AckBy       string            `json:"ack_by,omitempty"`
	AckAt       time.Time         `json:"ack_at,omitzero"`
	AckComment  string            `json:"ack_comment,omitempty"`
	Reopened    int               `json:"reopened"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Children    []Incident        `json:"children,omitempty"`
}

// Event is one entry of the history. Ids only grow, so "seen up to" is one
// number per person.
type Event struct {
	ID       int64     `json:"id"`
	Incident int64     `json:"incident"`
	Time     time.Time `json:"time"`
	Type     string    `json:"type"` // opened, resolved, reopened, acknowledged, silenced, unsilenced
	Detail   string    `json:"detail,omitempty"`
}

// Tracker keeps the incidents in the server's database.
type Tracker struct {
	db *sql.DB
	// Grace is how long a signal may be missing before its incident is
	// resolved; Reopen is how soon after that it comes back as the same
	// incident rather than a new one.
	Grace, Reopen time.Duration
}

var ErrNotFound = errors.New("no such incident")

// New creates the tables if needed.
func New(db *sql.DB) (*Tracker, error) {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS incidents (id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT NOT NULL, kind TEXT NOT NULL,
			resource TEXT NOT NULL, agent TEXT NOT NULL, title TEXT NOT NULL, severity TEXT NOT NULL, state TEXT NOT NULL,
			silenced INTEGER NOT NULL, parent INTEGER NOT NULL DEFAULT 0, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
			resolved_at INTEGER NOT NULL DEFAULT 0, ack_by TEXT NOT NULL DEFAULT '', ack_at INTEGER NOT NULL DEFAULT 0,
			ack_comment TEXT NOT NULL DEFAULT '', reopened INTEGER NOT NULL DEFAULT 0, labels TEXT NOT NULL, annotations TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS incidents_key ON incidents (key, resolved_at)`,
		`ALTER TABLE incidents ADD COLUMN parent_resource TEXT NOT NULL DEFAULT ''`, // added later
		`CREATE INDEX IF NOT EXISTS incidents_state ON incidents (state, last_seen)`,
		`CREATE TABLE IF NOT EXISTS incident_events (id INTEGER PRIMARY KEY AUTOINCREMENT, incident INTEGER NOT NULL,
			time INTEGER NOT NULL, type TEXT NOT NULL, detail TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS incident_events_incident ON incident_events (incident)`,
		// How far each person has looked at the history.
		`CREATE TABLE IF NOT EXISTS briefing_seen (user TEXT PRIMARY KEY, event_id INTEGER NOT NULL, at INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Tracker{db: db, Grace: time.Minute, Reopen: 30 * time.Minute}, nil
}

const columns = `id, key, kind, resource, parent_resource, agent, title, severity, state, silenced, parent, first_seen, last_seen,
	resolved_at, ack_by, ack_at, ack_comment, reopened, labels, annotations`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Incident, error) {
	var (
		in                           Incident
		silenced                     int
		first, last, resolved, ackAt int64
		labels, annotations          string
	)
	err := row.Scan(&in.ID, &in.Key, &in.Kind, &in.Resource, &in.ParentRes, &in.Agent, &in.Title, &in.Severity, &in.State, &silenced,
		&in.Parent, &first, &last, &resolved, &in.AckBy, &ackAt, &in.AckComment, &in.Reopened, &labels, &annotations)
	if err != nil {
		return in, err
	}
	in.Silenced = silenced != 0
	in.FirstSeen, in.LastSeen = unix(first), unix(last)
	in.ResolvedAt, in.AckAt = unix(resolved), unix(ackAt)
	json.Unmarshal([]byte(labels), &in.Labels)
	json.Unmarshal([]byte(annotations), &in.Annotations)
	return in, nil
}

func unix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func addEvent(tx *sql.Tx, incident int64, now time.Time, typ, detail string) error {
	_, err := tx.Exec(`INSERT INTO incident_events (incident, time, type, detail) VALUES (?, ?, ?, ?)`, incident, now.Unix(), typ, detail)
	return err
}

// Sync brings the incidents in line with the signals of now. Signals with
// the same key are one problem (the first one wins). With resolve false a
// missing signal resolves nothing: right after the server starts the
// alerts are not all back yet (Alertmanager keeps none across a restart,
// and rules send theirs again only when they next run).
func (t *Tracker) Sync(ctx context.Context, signals []Signal, now time.Time, resolve bool) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	open := map[string]Incident{}
	rows, err := tx.Query(`SELECT `+columns+` FROM incidents WHERE state != ?`, StateResolved)
	if err != nil {
		return err
	}
	for rows.Next() {
		in, err := scan(rows)
		if err != nil {
			rows.Close()
			return err
		}
		open[in.Key] = in
	}
	rows.Close()

	seen := map[string]bool{}
	for _, sig := range signals {
		if seen[sig.Key] {
			continue
		}
		seen[sig.Key] = true
		if in, ok := open[sig.Key]; ok {
			if in.Silenced != sig.Silenced {
				typ := "unsilenced"
				if sig.Silenced {
					typ = "silenced"
				}
				if err := addEvent(tx, in.ID, now, typ, ""); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE incidents SET last_seen = ?, silenced = ?, title = ?, severity = ?, annotations = ? WHERE id = ?`,
				now.Unix(), boolInt(sig.Silenced), sig.Title, sig.Severity, jsonText(sig.Annotations), in.ID); err != nil {
				return err
			}
			continue
		}
		// Back soon after it was resolved: the same incident again.
		var id int64
		err := tx.QueryRow(`SELECT id FROM incidents WHERE key = ? AND state = ? AND resolved_at >= ? ORDER BY resolved_at DESC LIMIT 1`,
			sig.Key, StateResolved, now.Add(-t.Reopen).Unix()).Scan(&id)
		switch {
		case err == nil:
			if _, err := tx.Exec(`UPDATE incidents SET state = ?, resolved_at = 0, last_seen = ?, silenced = ?, title = ?, severity = ?,
				annotations = ?, reopened = reopened + 1, ack_by = '', ack_at = 0, ack_comment = '' WHERE id = ?`,
				StateOpen, now.Unix(), boolInt(sig.Silenced), sig.Title, sig.Severity, jsonText(sig.Annotations), id); err != nil {
				return err
			}
			if err := addEvent(tx, id, now, "reopened", ""); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			first := sig.Since
			if first.IsZero() || first.After(now) {
				first = now
			}
			res, err := tx.Exec(`INSERT INTO incidents (key, kind, resource, parent_resource, agent, title, severity, state, silenced,
				first_seen, last_seen, labels, annotations) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sig.Key, sig.Kind, sig.Resource, sig.ParentResource, sig.Agent, sig.Title, sig.Severity, StateOpen, boolInt(sig.Silenced),
				first.Unix(), now.Unix(), jsonText(sig.Labels), jsonText(sig.Annotations))
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			if err := addEvent(tx, id, now, "opened", ""); err != nil {
				return err
			}
		default:
			return err
		}
	}

	for key, in := range open {
		if !resolve || seen[key] || now.Sub(in.LastSeen) < t.Grace {
			continue
		}
		if _, err := tx.Exec(`UPDATE incidents SET state = ?, resolved_at = ?, parent = 0 WHERE id = ?`, StateResolved, now.Unix(), in.ID); err != nil {
			return err
		}
		if err := addEvent(tx, in.ID, now, "resolved", ""); err != nil {
			return err
		}
	}

	// A host that is down holds the other open incidents of that host;
	// otherwise an open incident on what contains a resource holds it.
	if _, err := tx.Exec(`UPDATE incidents SET parent = COALESCE(
			(SELECT h.id FROM incidents h WHERE h.kind = ? AND h.state != ? AND h.agent = incidents.agent AND h.agent != ''
				ORDER BY h.id LIMIT 1),
			(SELECT p.id FROM incidents p WHERE p.state != ? AND incidents.parent_resource != '' AND p.resource = incidents.parent_resource
				AND p.id != incidents.id ORDER BY p.id LIMIT 1),
			0)
		WHERE state != ? AND kind != ?`, KindHostDown, StateResolved, StateResolved, StateResolved, KindHostDown); err != nil {
		return err
	}
	return tx.Commit()
}

// Acknowledge records that someone is on it. It lasts until the incident
// is resolved; a reopened incident needs it again.
func (t *Tracker) Acknowledge(ctx context.Context, id int64, user, comment string, now time.Time) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE incidents SET state = ?, ack_by = ?, ack_at = ?, ack_comment = ? WHERE id = ? AND state != ?`,
		StateAcknowledged, user, now.Unix(), comment, id, StateResolved)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := addEvent(tx, id, now, "acknowledged", strings.TrimSpace(user+": "+comment)); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *Tracker) list(ctx context.Context, where string, args ...any) ([]Incident, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT `+columns+` FROM incidents WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		in, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Open lists the incidents that are not resolved, most urgent first, with
// those held by another (a down host, a backend) nested under it.
func (t *Tracker) Open(ctx context.Context) ([]Incident, error) {
	all, err := t.list(ctx, `state != ?`, StateResolved)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Incident{}
	for i := range all {
		byID[all[i].ID] = &all[i]
	}
	var nest func(in Incident) Incident
	nest = func(in Incident) Incident {
		for _, c := range all {
			if c.Parent == in.ID {
				in.Children = append(in.Children, nest(c))
			}
		}
		Rank(in.Children)
		return in
	}
	top := []Incident{}
	for _, in := range all {
		if _, held := byID[in.Parent]; in.Parent == 0 || !held { // a parent may vanish between two syncs
			top = append(top, nest(in))
		}
	}
	Rank(top)
	return top, nil
}

// Resolved lists incidents resolved since then, newest first.
func (t *Tracker) Resolved(ctx context.Context, since time.Time, limit int) ([]Incident, error) {
	return t.list(ctx, `state = ? AND resolved_at >= ? ORDER BY resolved_at DESC LIMIT ?`, StateResolved, since.Unix(), limit)
}

// Around lists the incidents of an agent that were open at some point
// between from and to, newest first.
func (t *Tracker) Around(ctx context.Context, agent string, from, to time.Time) ([]Incident, error) {
	return t.list(ctx, `agent = ? AND first_seen <= ? AND (resolved_at = 0 OR resolved_at >= ?) ORDER BY first_seen DESC LIMIT 20`,
		agent, to.Unix(), from.Unix())
}

// OfResource lists the incidents of a resource (an SLO's "slo:<name>")
// that were open at some point since then, newest first.
func (t *Tracker) OfResource(ctx context.Context, resource string, since time.Time, limit int) ([]Incident, error) {
	return t.list(ctx, `resource = ? AND (resolved_at = 0 OR resolved_at >= ?) ORDER BY first_seen DESC LIMIT ?`,
		resource, since.Unix(), limit)
}

// Get returns one incident and its history.
func (t *Tracker) Get(ctx context.Context, id int64) (Incident, []Event, error) {
	list, err := t.list(ctx, `id = ?`, id)
	if err != nil {
		return Incident{}, nil, err
	}
	if len(list) == 0 {
		return Incident{}, nil, ErrNotFound
	}
	events, err := t.events(ctx, `incident = ? ORDER BY id`, id)
	return list[0], events, err
}

func (t *Tracker) events(ctx context.Context, where string, args ...any) ([]Event, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT id, incident, time, type, detail FROM incident_events WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e  Event
			ts int64
		)
		if err := rows.Scan(&e.ID, &e.Incident, &ts, &e.Type, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = unix(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Since tells what happened after event id after: how many incidents were
// opened (or reopened) and resolved, and the newest event id.
type Since struct {
	After    int64 `json:"after"`
	Latest   int64 `json:"latest"`
	Opened   int   `json:"opened"`
	Resolved int   `json:"resolved"`
}

func (t *Tracker) Since(ctx context.Context, after int64) (Since, error) {
	out := Since{After: after}
	err := t.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0),
			COUNT(DISTINCT CASE WHEN type IN ('opened', 'reopened') AND id > ? THEN incident END),
			COUNT(DISTINCT CASE WHEN type = 'resolved' AND id > ? THEN incident END)
		FROM incident_events`, after, after).Scan(&out.Latest, &out.Opened, &out.Resolved)
	return out, err
}

// Seen returns how far a person has looked; 0 for someone who never did.
func (t *Tracker) Seen(ctx context.Context, user string) (int64, error) {
	var id int64
	err := t.db.QueryRowContext(ctx, `SELECT event_id FROM briefing_seen WHERE user = ?`, user).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// MarkSeen moves a person's marker to the event id they were shown. It
// never moves back, so an older page sent late changes nothing.
func (t *Tracker) MarkSeen(ctx context.Context, user string, eventID int64, now time.Time) error {
	_, err := t.db.ExecContext(ctx, `INSERT INTO briefing_seen (user, event_id, at) VALUES (?, ?, ?)
		ON CONFLICT(user) DO UPDATE SET event_id = MAX(event_id, excluded.event_id), at = excluded.at`, user, eventID, now.Unix())
	return err
}

// Prune deletes incidents resolved before then, and their history.
func (t *Tracker) Prune(ctx context.Context, before time.Time) error {
	_, err := t.db.ExecContext(ctx, `DELETE FROM incident_events WHERE incident IN
		(SELECT id FROM incidents WHERE state = ? AND resolved_at < ?)`, StateResolved, before.Unix())
	if err == nil {
		_, err = t.db.ExecContext(ctx, `DELETE FROM incidents WHERE state = ? AND resolved_at < ?`, StateResolved, before.Unix())
	}
	return err
}

// Noisy is an alert that keeps coming back: a candidate for tuning (a
// `for`, `keep_firing_for`, a better threshold or a silence), which is the
// operator's call; vigil only shows the history.
type Noisy struct {
	Alertname string  `json:"alertname"`
	Resource  string  `json:"resource"`
	Title     string  `json:"title"`
	Firings   int     `json:"firings"`
	MedianMin float64 `json:"median_minutes"` // how long it fired, the median of the firings that ended
	Incident  int64   `json:"incident"`       // the latest, to see its history
}

// Noisy lists alerts that fired at least min times since then, counted as
// openings and reopenings, most frequent first.
func (t *Tracker) Noisy(ctx context.Context, since time.Time, min int) ([]Noisy, error) {
	rows, err := t.db.QueryContext(ctx, `SELECT e.incident, e.time, e.type, i.resource, i.title, i.labels
		FROM incident_events e JOIN incidents i ON i.id = e.incident
		WHERE e.time >= ? AND e.type IN ('opened', 'reopened', 'resolved') ORDER BY e.id`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		Noisy
		durations []float64
	}
	groups := map[string]*acc{}
	started := map[int64]int64{}
	for rows.Next() {
		var (
			id, ts                       int64
			typ, resource, title, labels string
		)
		if err := rows.Scan(&id, &ts, &typ, &resource, &title, &labels); err != nil {
			return nil, err
		}
		var l map[string]string
		json.Unmarshal([]byte(labels), &l)
		k := l["alertname"] + "\x00" + resource
		g := groups[k]
		if g == nil {
			g = &acc{Noisy: Noisy{Alertname: l["alertname"], Resource: resource}}
			groups[k] = g
		}
		g.Title, g.Incident = title, max(g.Incident, id)
		switch typ {
		case "resolved":
			if st, ok := started[id]; ok {
				g.durations = append(g.durations, float64(ts-st)/60)
				delete(started, id)
			}
		default:
			g.Firings++
			started[id] = ts
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Noisy{}
	for _, g := range groups {
		if g.Firings < min {
			continue
		}
		if n := len(g.durations); n > 0 {
			sort.Float64s(g.durations)
			g.MedianMin = g.durations[n/2]
		}
		out = append(out, g.Noisy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Firings > out[j].Firings })
	return out, nil
}

var severityRank = map[string]int{"critical": 0, "error": 1, "warning": 2, "info": 3}

func rank(in Incident) int {
	if r, ok := severityRank[in.Severity]; ok {
		return r
	}
	return 4
}

// Rank orders incidents by urgency: unsilenced before silenced, open
// before acknowledged, then by severity, newest first among equals.
func Rank(list []Incident) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Silenced != b.Silenced {
			return !a.Silenced
		}
		if (a.State == StateOpen) != (b.State == StateOpen) {
			return a.State == StateOpen
		}
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		return a.FirstSeen.After(b.FirstSeen)
	})
}
