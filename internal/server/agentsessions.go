package server

import (
	"context"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
)

// Live state of AI coding sessions: whether each Claude Code session is
// working, idle, or waiting on its owner (a permission prompt or a
// question), across every machine. The owner usually is not watching the
// terminal, and a session that waits for an answer gets nothing done, so
// vigil can say so where they are (the built-in rule in ai_module_claude).
//
// The source is vigil-agent claude-hook: Claude Code's telemetry reports a
// permission only once it is decided, the hook as it is asked. The state
// is the newest of the events that change it, read with how recent the
// session's last line of any kind is:
//
//	UserPromptSubmit, PermissionDenied            working
//	PermissionRequest, Notification (permission   waiting on you, unless
//	  prompt or question)                         anything came after it
//	Stop, SessionStart, Notification (idle)       idle, unless anything came after
//	SessionEnd                                    ended
//
// A session that is working but has sent nothing for quietAfter is quiet:
// a long command, or a terminal closed without SessionEnd.

const (
	sessionWindow = 12 * time.Hour
	quietAfter    = 10 * time.Minute
	endedKept     = time.Hour // an ended session stays in the list this long
)

// stateEvents are the hook events that change a session's state.
var stateEvents = []string{"UserPromptSubmit", "PermissionRequest", "PermissionDenied", "Notification", "Stop", "SessionStart", "SessionEnd"}

// LiveSession is one session's state now.
type LiveSession struct {
	Agent      string    `json:"agent"`
	Session    string    `json:"session_id"`
	User       string    `json:"os_user,omitempty"`
	Project    string    `json:"project,omitempty"` // the last part of the folder it runs in
	Cwd        string    `json:"cwd,omitempty"`
	State      string    `json:"state"` // working, waiting, idle, quiet, ended
	Since      time.Time `json:"since"`
	WaitingFor string    `json:"waiting_for,omitempty"` // "permission: Bash", "a question", …
	Subagent   string    `json:"subagent,omitempty"`    // the subagent that asked, when not the main conversation
	LastSeen   time.Time `json:"last_seen"`
}

type liveSessions struct {
	mu   sync.Mutex
	list []LiveSession
	at   time.Time
	err  string
}

func (s *Server) agentSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/sessions/live", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		s.live.mu.Lock()
		list, at, msg := s.live.list, s.live.at, s.live.err
		s.live.mu.Unlock()
		if at.IsZero() || time.Since(at) > time.Minute { // not run yet, or the loop is behind
			var err error
			if list, err = s.readLiveSessions(r.Context(), time.Now()); err != nil {
				http.Error(w, "the log store did not answer: "+err.Error(), http.StatusBadGateway)
				return
			}
			at, msg = time.Now(), ""
		}
		if list == nil {
			list = []LiveSession{}
		}
		writeJSON(w, map[string]any{"sessions": list, "at": at, "error": msg})
	}))
}

// readLiveSessions asks the log store for the newest state-changing event
// and the newest line of every session seen in the last sessionWindow.
func (s *Server) readLiveSessions(ctx context.Context, now time.Time) ([]LiveSession, error) {
	if s.logs == nil {
		return nil, nil
	}
	names := make([]string, len(stateEvents))
	for i, e := range stateEvents {
		names[i] = `"claude_code.hook.` + e + `"`
	}
	from := now.Add(-sessionWindow)
	events, err := s.logQuery(ctx, `service:"claude-code-hook" _msg:in(`+strings.Join(names, ",")+`) | sort by (_time desc) | limit 5000`, from, now, 5000)
	if err != nil {
		return nil, err
	}
	last, err := s.logQuery(ctx, `service:"claude-code-hook" | stats by (agent, session_id) max(_time) as last`, from, now, 5000)
	if err != nil {
		return nil, err
	}
	return liveStates(events, last, now), nil
}

// liveStates works the states out of the store's rows: events newest first,
// and the newest line of each session.
func liveStates(events, last []map[string]string, now time.Time) []LiveSession {
	key := func(r map[string]string) string { return r["agent"] + "\x00" + r["session_id"] }
	seen := map[string]time.Time{}
	for _, r := range last {
		if t, err := time.Parse(time.RFC3339Nano, r["last"]); err == nil {
			seen[key(r)] = t
		}
	}
	done := map[string]bool{}
	var out []LiveSession
	for _, e := range events {
		k := key(e)
		if e["session_id"] == "" || done[k] {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, e["_time"])
		if err != nil {
			continue
		}
		ev := strings.TrimPrefix(e["_msg"], "claude_code.hook.")
		note := e["notification_type"]
		if ev == "Notification" && note != "permission_prompt" && note != "elicitation_dialog" && note != "idle_prompt" {
			continue // auth_success and others say nothing about whose turn it is
		}
		done[k] = true
		ls := LiveSession{Agent: e["agent"], Session: e["session_id"], User: e["os_user"], Cwd: e["cwd"], Since: at, LastSeen: at}
		if ls.Cwd != "" {
			ls.Project = path.Base(strings.ReplaceAll(ls.Cwd, `\`, "/"))
		}
		if t := seen[k]; t.After(ls.LastSeen) {
			ls.LastSeen = t
		}
		// Lines that come with or right after the event itself (the
		// subagent's own, a notification next to its permission request)
		// are not an answer.
		later := ls.LastSeen.Sub(at) > 2*time.Second
		switch {
		case ev == "SessionEnd":
			ls.State = "ended"
		case ev == "PermissionRequest" || ev == "Notification" && note != "idle_prompt":
			if later {
				ls.State, ls.Since = "working", ls.LastSeen
				break
			}
			ls.State = "waiting"
			switch {
			case ev == "PermissionRequest" && e["tool_name"] != "":
				ls.WaitingFor = "permission: " + e["tool_name"]
			case ev == "PermissionRequest" || note == "permission_prompt":
				ls.WaitingFor = "a permission"
			default:
				ls.WaitingFor = "an answer to a question"
			}
			if e["agent_type"] != "" {
				ls.Subagent = e["agent_type"]
			}
		case ev == "Stop" || ev == "SessionStart" || ev == "Notification":
			if later { // background work after the turn ended
				ls.State, ls.Since = "working", ls.LastSeen
			} else {
				ls.State = "idle"
			}
		default:
			ls.State = "working"
		}
		if ls.State == "working" && now.Sub(ls.LastSeen) > quietAfter {
			ls.State, ls.Since = "quiet", ls.LastSeen
		}
		if ls.State == "ended" && now.Sub(at) > endedKept {
			continue
		}
		out = append(out, ls)
	}
	rank := map[string]int{"waiting": 0, "working": 1, "idle": 2, "quiet": 3, "ended": 4}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// liveSessionLoop keeps the states fresh and writes
// vigil_ai_session_waiting_seconds{tool, agent, session_id, os_user,
// project, waiting_for} for each session waiting on its owner, which the
// ai_module_claude rule ClaudeCodeWaitingOnYou alerts on.
func (s *Server) liveSessionLoop(ctx context.Context) {
	if s.logs == nil {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			list, err := s.readLiveSessions(ctx, now)
			s.live.mu.Lock()
			if err != nil {
				s.live.err = err.Error()
			} else {
				s.live.list, s.live.err = list, ""
			}
			s.live.at = now
			s.live.mu.Unlock()
			if err != nil {
				continue
			}
			var samples []model.Sample
			for _, ls := range list {
				if ls.State != "waiting" {
					continue
				}
				samples = append(samples, model.Sample{Name: "vigil_ai_session_waiting_seconds", Labels: map[string]string{
					"tool": "claude_code", "agent": ls.Agent, "session_id": ls.Session, "os_user": ls.User,
					"project": ls.Project, "waiting_for": ls.WaitingFor}, Value: now.Sub(ls.Since).Seconds(), TS: now.UnixMilli()})
			}
			if len(samples) == 0 {
				continue
			}
			if err := s.forward(ctx, "", promwire.Encode(samples)); err != nil && ctx.Err() == nil {
				s.log.Warn("writing session states failed", "err", err)
			}
		}
	}
}
