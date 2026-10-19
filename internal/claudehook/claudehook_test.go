package claudehook

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// testdata/hooks.jsonl is every hook call of one real Claude Code run
// (2.1.303, -p, an Explore subagent, `cat .env`, a failing `cat`),
// scrubbed of names, paths and ids.
func inputs(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
	}
	return out
}

func attrs(rec *logspb.LogRecord) map[string]string {
	m := map[string]string{}
	for _, kv := range rec.GetAttributes() {
		m[kv.Key] = kv.GetValue().GetStringValue()
	}
	return m
}

func TestRecords(t *testing.T) {
	seen := map[string]int{}
	for _, raw := range inputs(t) {
		req, err := Record(raw, time.Unix(1700000000, 0), Options{User: "dev"})
		if err != nil {
			t.Fatal(err)
		}
		rl := req.GetResourceLogs()[0]
		rec := rl.GetScopeLogs()[0].GetLogRecords()[0]
		a := attrs(rec)
		event := a["hook_event"]
		seen[event]++
		if body := rec.GetBody().GetStringValue(); body != "claude_code.hook."+event {
			t.Errorf("body %q for %s", body, event)
		}
		if a["session.id"] == "" || a["os_user"] != "dev" || a["cwd"] != "/home/user/work" {
			t.Errorf("%s: %v", event, a)
		}
		// What a tool printed is never sent, only how much: here it holds
		// the .env file.
		j, _ := protojson.Marshal(req)
		if strings.Contains(string(j), "example-not-a-secret") {
			t.Errorf("%s carries tool output: %s", event, j)
		}
		// Without -content, no prompt text and no subagent instructions.
		if a["prompt"] != "" || a["tool.prompt"] != "" {
			t.Errorf("%s carries text without -content", event)
		}
		switch event {
		case "UserPromptSubmit":
			if a["prompt_length"] == "" || a["prompt.id"] == "" {
				t.Errorf("prompt: %v", a)
			}
		case "SessionStart":
			if a["start_source"] != "startup" {
				t.Errorf("session start: %v", a)
			}
		case "PostToolUseFailure":
			if rec.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || !strings.Contains(a["error"], "missing.txt") || a["tool.command"] != "cat missing.txt" {
				t.Errorf("failure: %v", a)
			}
		case "PostToolUse":
			if a["tool_name"] == "Bash" && a["tool.command"] == "" {
				t.Errorf("a Bash call without its command: %v", a)
			}
			if a["tool_name"] == "Agent" && a["tool.subagent_type"] != "Explore" {
				t.Errorf("Agent call: %v", a)
			}
			if a["tool.response_bytes"] == "" || a["duration_ms"] == "" {
				t.Errorf("tool call without sizes: %v", a)
			}
		case "SubagentStart", "SubagentStop":
			if a["agent_id"] == "" || a["agent_type"] != "Explore" {
				t.Errorf("subagent: %v", a)
			}
		}
	}
	for _, e := range []string{"SessionStart", "UserPromptSubmit", "SubagentStart", "PostToolUse", "PostToolUseFailure", "SubagentStop", "SessionEnd"} {
		if seen[e] == 0 {
			t.Errorf("no %s in the fixture", e)
		}
	}
}

func TestContent(t *testing.T) {
	var prompt, instructions bool
	for _, raw := range inputs(t) {
		req, _ := Record(raw, time.Now(), Options{Content: true})
		a := attrs(req.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0])
		prompt = prompt || strings.HasPrefix(a["prompt"], "Use an Explore subagent")
		instructions = instructions || strings.HasPrefix(a["tool.prompt"], "Working directory:")
	}
	if !prompt || !instructions {
		t.Errorf("with -content: prompt %v, subagent instructions %v", prompt, instructions)
	}
	long := `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"` + strings.Repeat("x", 5000) + `"}`
	req, _ := Record([]byte(long), time.Now(), Options{Content: true})
	if a := attrs(req.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]); len([]rune(a["prompt"])) != maxText+1 || a["prompt_length"] != "5000" {
		t.Errorf("long prompt: %d characters kept, length %s", len([]rune(a["prompt"])), a["prompt_length"])
	}
}

func TestBadInput(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"session_id":"s"}`} {
		if _, err := Record([]byte(raw), time.Now(), Options{}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestSend(t *testing.T) {
	var got *collogs.ExportLogsServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			http.Error(w, "wrong", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = &collogs.ExportLogsServiceRequest{}
		proto.Unmarshal(b, got)
	}))
	defer srv.Close()
	req, _ := Record(inputs(t)[0], time.Now(), Options{})
	if err := Send(context.Background(), srv.URL+"/", req); err != nil || got == nil {
		t.Fatalf("send: %v", err)
	}
	if got.GetResourceLogs()[0].GetResource().GetAttributes()[0].GetValue().GetStringValue() != Service {
		t.Error("service name lost")
	}
	// An agent that does not answer in time costs the hook its timeout,
	// not Claude Code's.
	slow, err := net.Listen("tcp", "127.0.0.1:0") // takes connections, never answers
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := Send(ctx, "http://"+slow.Addr().String(), req); err == nil || time.Since(start) > time.Second {
		t.Errorf("slow agent: %v after %v", err, time.Since(start))
	}
}
