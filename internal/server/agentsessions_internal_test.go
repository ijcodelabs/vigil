package server

import (
	"testing"
	"time"
)

// Each session's state from its newest state-changing event and its newest
// line of any kind.
func TestLiveStates(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	ev := func(session, event string, ago time.Duration, extra ...string) map[string]string {
		m := map[string]string{"agent": "mac", "session_id": session, "_msg": "claude_code.hook." + event, "_time": at(ago),
			"cwd": "/home/dev/shop-api", "os_user": "dev"}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	last := func(session string, ago time.Duration) map[string]string {
		return map[string]string{"agent": "mac", "session_id": session, "last": at(ago)}
	}
	// Newest first, as the store answers.
	events := []map[string]string{
		ev("ask", "PermissionRequest", 4*time.Minute, "tool_name", "Bash"),
		ev("answered", "PermissionRequest", 6*time.Minute, "tool_name", "Edit"),
		ev("question", "Notification", 3*time.Minute, "notification_type", "elicitation_dialog", "agent_type", "Explore"),
		ev("done", "Stop", 2*time.Minute),
		ev("busy", "UserPromptSubmit", 1*time.Minute),
		ev("stuck", "UserPromptSubmit", 40*time.Minute),
		ev("closed", "SessionEnd", 5*time.Minute),
		ev("old", "SessionEnd", 3*time.Hour),
		ev("login", "Notification", 30*time.Second, "notification_type", "auth_success"), // says nothing
		ev("login", "UserPromptSubmit", 20*time.Minute),
		ev("ask", "UserPromptSubmit", 10*time.Minute), // older than its PermissionRequest: ignored
	}
	lasts := []map[string]string{
		last("ask", 4*time.Minute), last("answered", 5*time.Minute), last("question", 3*time.Minute),
		last("done", 2*time.Minute), last("busy", 10*time.Second), last("stuck", 40*time.Minute),
		last("login", 30*time.Second),
	}
	got := map[string]LiveSession{}
	var order []string
	for _, s := range liveStates(events, lasts, now) {
		got[s.Session] = s
		order = append(order, s.Session)
	}
	want := map[string]string{"ask": "waiting", "question": "waiting", "answered": "working", "busy": "working",
		"login": "working", "done": "idle", "stuck": "quiet", "closed": "ended"}
	for id, state := range want {
		if got[id].State != state {
			t.Errorf("%s: %s, want %s (%+v)", id, got[id].State, state, got[id])
		}
	}
	if _, ok := got["old"]; ok {
		t.Errorf("a session that ended hours ago is listed")
	}
	if s := got["ask"]; s.WaitingFor != "permission: Bash" || !s.Since.Equal(now.Add(-4*time.Minute)) || s.Project != "shop-api" {
		t.Errorf("ask: %+v", s)
	}
	if s := got["question"]; s.WaitingFor != "an answer to a question" || s.Subagent != "Explore" {
		t.Errorf("question: %+v", s)
	}
	if order[0] != "question" && order[0] != "ask" || order[len(order)-1] != "closed" {
		t.Errorf("order: %v (waiting first, ended last)", order)
	}
}
