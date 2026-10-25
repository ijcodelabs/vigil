package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The context of a log line is what a person would otherwise look up by
// hand before deciding whether it matters: how often lines like it come
// and since when, the lines around it, and what happened on that host at
// that moment (an agent restart, a container that stopped, a profile
// change, an open incident). It needs no AI; an explanation is built on
// top of it (logexplain.go).

const (
	contextAround = 10               // lines before and after
	contextWindow = 10 * time.Minute // host events this far on either side
)

// LogContext answers POST /api/v1/loglines/context.
type LogContext struct {
	Time      time.Time     `json:"time"`
	Agent     string        `json:"agent"`
	Source    string        `json:"source"` // e.g. unit docker.service; empty when the line has no named source
	Level     string        `json:"level,omitempty"`
	Pattern   string        `json:"pattern"`
	Like      LikeThis      `json:"like"`
	Before    []ContextLine `json:"before"`
	After     []ContextLine `json:"after"`
	Host      []HostEvent   `json:"host"`
	Incidents []ContextInc  `json:"incidents"`
	Notes     []string      `json:"notes"`
	Errors    []string      `json:"errors,omitempty"` // parts that could not be read
}

// LikeThis counts lines of the same source matching the pattern.
type LikeThis struct {
	Hour   int          `json:"hour"`
	Day    int          `json:"day"`
	Week   int          `json:"week"`
	First  string       `json:"first,omitempty"`
	Last   string       `json:"last,omitempty"`
	Agents []AgentCount `json:"agents"`
}

type AgentCount struct {
	Agent string `json:"agent"`
	Count int    `json:"count"`
}

type ContextLine struct {
	Time  string `json:"time"`
	Msg   string `json:"msg"`
	Level string `json:"level,omitempty"`
}

type HostEvent struct {
	Time time.Time `json:"time"`
	What string    `json:"what"`
}

type ContextInc struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Severity string    `json:"severity,omitempty"`
	State    string    `json:"state"`
	Since    time.Time `json:"since"`
	Resolved time.Time `json:"resolved,omitzero"`
}

var (
	uuidRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hexRe      = regexp.MustCompile(`(?i)\b[0-9a-f]{8,}\b`)
	digitsRe   = regexp.MustCompile(`\d+`)
	streamIDRe = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	errorish   = regexp.MustCompile(`(?i)\b(error|err|fail|failed|failure|fatal|panic|exception)\b`)
	quiet      = map[string]bool{"info": true, "information": true, "notice": true, "debug": true, "trace": true}
)

// linePattern makes a regular expression for "lines like this": the text
// taken literally, with what changes from line to line (ids, numbers)
// replaced by what it may be. It does what suggestPattern does in the
// interface (explore.js).
func linePattern(msg string) string {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	p := regexp.QuoteMeta(msg)
	p = uuidRe.ReplaceAllString(p, "\x01")
	p = hexRe.ReplaceAllStringFunc(p, func(h string) string {
		if strings.ContainsAny(h, "0123456789") && strings.ContainsAny(strings.ToLower(h), "abcdef") {
			return "\x02"
		}
		return h
	})
	p = digitsRe.ReplaceAllString(p, `\d+`)
	return strings.NewReplacer("\x01", "[0-9a-f-]+", "\x02", "[0-9a-f]+").Replace(p)
}

// lineSource names where a line came from, as a stream filter.
func lineSource(row map[string]string) (field, name string) {
	for _, f := range []string{"container", "pod", "unit", "app"} {
		if v := row[f]; v != "" {
			return f, v
		}
	}
	return "", ""
}

type logLineRequest struct {
	Line  map[string]string `json:"line"`
	Again bool              `json:"again"` // explain again instead of answering from the cache
}

// readLogLine checks what the interface sends about a line.
func readLogLine(w http.ResponseWriter, r *http.Request) (logLineRequest, time.Time, bool) {
	var in logLineRequest
	if !readJSON(w, r, &in) {
		return in, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, in.Line["_time"])
	switch {
	case err != nil:
		http.Error(w, "the line has no readable _time", http.StatusBadRequest)
	case in.Line["agent"] == "":
		http.Error(w, "the line names no agent", http.StatusBadRequest)
	case !streamIDRe.MatchString(in.Line["_stream_id"]):
		http.Error(w, "the line has no _stream_id", http.StatusBadRequest)
	default:
		return in, at, true
	}
	return in, at, false
}

func (s *Server) logLineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/loglines/context", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		in, at, ok := readLogLine(w, r)
		if ok {
			writeJSON(w, s.logContext(r.Context(), in.Line, at, time.Now()))
		}
	}))
	mux.HandleFunc("POST /api/v1/loglines/explain/preview", s.admin(s.handleLogExplainPreview))
	mux.HandleFunc("POST /api/v1/loglines/explain", s.admin(s.handleLogExplain))
}

// logContext gathers the context of one line. A part the stores cannot
// answer is left empty and named in Errors; the rest is still useful.
func (s *Server) logContext(ctx context.Context, row map[string]string, at, now time.Time) LogContext {
	agent := row["agent"]
	c := LogContext{Time: at, Agent: agent, Level: row["level"], Pattern: linePattern(row["_msg"]),
		Before: []ContextLine{}, After: []ContextLine{}, Host: []HostEvent{}, Incidents: []ContextInc{}, Notes: []string{}}
	q := strconv.Quote
	fail := func(what string, err error) { c.Errors = append(c.Errors, what+": "+err.Error()) }

	// Lines like it, from the same source on any agent.
	field, name := lineSource(row)
	scope := "_stream_id:" + row["_stream_id"]
	if field != "" {
		c.Source = field + " " + name
		scope = field + ":=" + q(name)
	}
	rows, err := s.logQuery(ctx, fmt.Sprintf(`%s _msg:~%s | stats by (agent) count() week, count() if (_time:24h) day, count() if (_time:1h) hour, min(_time) first, max(_time) last`,
		scope, q(c.Pattern)), now.Add(-7*24*time.Hour), now, 1000)
	if err != nil {
		fail("lines like it", err)
	}
	for _, r := range rows {
		n, _ := strconv.Atoi(r["week"])
		d, _ := strconv.Atoi(r["day"])
		h, _ := strconv.Atoi(r["hour"])
		c.Like.Week, c.Like.Day, c.Like.Hour = c.Like.Week+n, c.Like.Day+d, c.Like.Hour+h
		if f := r["first"]; f != "" && (c.Like.First == "" || f < c.Like.First) {
			c.Like.First = f
		}
		if l := r["last"]; l > c.Like.Last {
			c.Like.Last = l
		}
		c.Like.Agents = append(c.Like.Agents, AgentCount{r["agent"], n})
	}
	sort.Slice(c.Like.Agents, func(i, j int) bool { return c.Like.Agents[i].Count > c.Like.Agents[j].Count })

	// The lines around it in its own stream.
	stream := "_stream_id:" + row["_stream_id"]
	same := func(l map[string]string) bool { return l["_time"] == row["_time"] && l["_msg"] == row["_msg"] }
	line := func(l map[string]string) ContextLine { return ContextLine{l["_time"], l["_msg"], l["level"]} }
	if before, err := s.logQuery(ctx, stream+" | sort by (_time desc)", at.Add(-time.Hour), at, contextAround+1); err != nil {
		fail("lines before", err)
	} else {
		for i := len(before) - 1; i >= 0; i-- {
			if !same(before[i]) && len(c.Before) < contextAround {
				c.Before = append(c.Before, line(before[i]))
			}
		}
		if len(c.Before) > contextAround {
			c.Before = c.Before[len(c.Before)-contextAround:]
		}
	}
	if after, err := s.logQuery(ctx, stream+" | sort by (_time)", at, at.Add(time.Hour), contextAround+1); err != nil {
		fail("lines after", err)
	} else {
		for _, l := range after {
			if !same(l) && len(c.After) < contextAround {
				c.After = append(c.After, line(l))
			}
		}
	}

	// What happened on the host around it.
	from, to := at.Add(-contextWindow), at.Add(contextWindow)
	sel := "{agent=" + q(agent) + "}"
	// Every 15 s, so that two starts within a minute are both seen.
	starts, err := s.queryRange(ctx, "vigil_agent_start_time_seconds"+sel, from, to.Add(2*time.Minute), 15*time.Second)
	if err != nil {
		fail("agent starts", err)
	}
	seen := map[int64]bool{}
	for _, sr := range starts {
		for _, p := range sr.Points {
			t := int64(p.V)
			if !seen[t] && t >= from.Unix() && t <= to.Unix() {
				seen[t] = true
				c.Host = append(c.Host, HostEvent{time.Unix(t, 0).UTC(), "the vigil agent on this host started"})
			}
		}
	}
	if running, err := s.queryRange(ctx, "container_running"+sel, from, to, time.Minute); err != nil {
		fail("containers", err)
	} else {
		for _, sr := range running {
			for i := 1; i < len(sr.Points); i++ {
				if was, is := sr.Points[i-1].V, sr.Points[i].V; was != is {
					what := "stopped"
					if is == 1 {
						what = "started"
					}
					c.Host = append(c.Host, HostEvent{sr.Points[i].T.UTC(), fmt.Sprintf("container %s %s (within the minute before)", sr.Labels["container"], what)})
				}
			}
		}
	}
	bumps := func(expr, what string) {
		series, err := s.queryRange(ctx, expr, from, to, time.Minute)
		if err != nil {
			fail(what, err)
			return
		}
		for _, sr := range series {
			for _, p := range sr.Points {
				if p.V >= 1 {
					c.Host = append(c.Host, HostEvent{p.T.UTC(), fmt.Sprintf(what, sr.Labels["container"], p.V)})
				}
			}
		}
	}
	bumps("increase(container_restarts_total"+sel+"[1m]) > 0", "container %s restarted (%[2]g times in the minute before)")
	bumps("increase(host_oom_kills_total"+sel+"[1m]) > 0", "the kernel killed a process for memory%.0s (%g in the minute before)")
	if prof, err := s.store.db.QueryContext(ctx, `SELECT name, version, author, time, deleted FROM profile_versions WHERE time BETWEEN ? AND ? ORDER BY time`,
		at.Add(-time.Hour).Unix(), to.Unix()); err != nil {
		fail("profile changes", err)
	} else {
		for prof.Next() {
			var (
				pname, author string
				version, ts   int64
				deleted       bool
			)
			if prof.Scan(&pname, &version, &author, &ts, &deleted) == nil {
				what := "saved"
				if deleted {
					what = "deleted"
				}
				c.Host = append(c.Host, HostEvent{time.Unix(ts, 0).UTC(), fmt.Sprintf("profile %s v%d %s by %s", pname, version, what, author)})
			}
		}
		prof.Close()
	}
	sort.Slice(c.Host, func(i, j int) bool { return c.Host[i].Time.Before(c.Host[j].Time) })

	if list, err := s.incidents.Around(ctx, agent, from, to); err != nil {
		fail("incidents", err)
	} else {
		for _, in := range list {
			c.Incidents = append(c.Incidents, ContextInc{in.ID, in.Title, in.Severity, in.State, in.FirstSeen, in.ResolvedAt})
		}
	}

	// What stands out, said plainly.
	if lvl := strings.ToLower(c.Level); quiet[lvl] && errorish.MatchString(row["_msg"]) {
		c.Notes = append(c.Notes, fmt.Sprintf("The text mentions an error, but the line's level is %s: whatever wrote it did not count it as one.", c.Level))
	}
	switch {
	case c.Like.Week == 1:
		c.Notes = append(c.Notes, "This is the only line like it in the last 7 days.")
	case c.Like.Week > 1 && c.Like.First != "":
		c.Notes = append(c.Notes, fmt.Sprintf("Lines like it came %d times in the last 7 days, the first at %s.", c.Like.Week, c.Like.First))
	}
	closest := 2 * time.Minute // the agent start nearest the line, if one is that near
	for _, e := range c.Host {
		if d := e.Time.Sub(at); d.Abs() < closest.Abs() && strings.HasPrefix(e.What, "the vigil agent") {
			closest = d
		}
	}
	if closest.Abs() < 2*time.Minute {
		c.Notes = append(c.Notes, fmt.Sprintf("The vigil agent on this host started %s this line: a line written while it stopped or started may come from that.", near(closest)))
	}
	return c
}

// near says how far an event was from the line, in words.
func near(d time.Duration) string {
	s := d.Round(time.Second)
	switch {
	case s == 0:
		return "in the same second as"
	case s > 0:
		return s.String() + " after"
	default:
		return (-s).String() + " before"
	}
}
