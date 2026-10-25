package otlp

import (
	"os"
	"strings"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/otlpwire"
	"github.com/ijcodelabs/vigil/internal/redact"
)

// The fixtures in testdata/claude_code are one real Claude Code session
// (2.1.303, every content flag on, one Explore subagent, a `cat .env`),
// recorded as sent and scrubbed of names, accounts and paths.
func fixture(t *testing.T, name string, m proto.Message) {
	t.Helper()
	b, err := os.ReadFile("testdata/claude_code/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCodeEvents(t *testing.T) {
	in := &collogs.ExportLogsServiceRequest{}
	fixture(t, "logs.json", in)
	records := 0
	for _, rl := range in.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			records += len(sl.GetLogRecords())
		}
	}
	entries := convertLogs(&otlpwire.LogsRequest{ResourceLogs: in.ResourceLogs})
	// Claude Code puts the event name in the body, so none is skipped.
	if len(entries) != records {
		t.Fatalf("%d of %d events kept", len(entries), records)
	}
	prompts := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Msg, "claude_code.") {
			t.Errorf("message %q is not an event name", e.Msg)
		}
		if e.Stream["service"] != "claude-code" {
			t.Errorf("stream %v", e.Stream)
		}
		if e.Msg == "claude_code.tool_decision" && e.Fields["attr_source"] != "config" {
			t.Errorf("tool_decision lost who decided: %v", e.Fields)
		}
		if e.Msg == "claude_code.user_prompt" {
			prompts++
			if e.Fields["prompt_id"] == "" || e.Fields["session_id"] == "" || e.Fields["trace_id"] == "" {
				t.Errorf("a prompt without its ids: %v", e.Fields)
			}
		}
	}
	if prompts != 1 {
		t.Errorf("%d prompts, want 1", prompts)
	}
}

func TestClaudeCodeMetricsDropLabels(t *testing.T) {
	in := &colmetrics.ExportMetricsServiceRequest{}
	fixture(t, "metrics.json", in)
	var want float64
	for _, rm := range in.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() == "claude_code.token.usage" {
					for _, p := range m.GetSum().GetDataPoints() {
						want += number(p)
					}
				}
			}
		}
	}
	drop := map[string]bool{"session_id": true, "user_id": true, "user_account_uuid": true, "user_account_id": true, "organization_id": true, "terminal_type": false}
	r := &Receiver{DropLabels: drop}
	latest := map[uint64]float64{}
	for _, s := range r.convertMetrics(&otlpwire.MetricsRequest{ResourceMetrics: in.ResourceMetrics}) {
		for k := range drop {
			if _, has := s.Labels[k]; has && drop[k] {
				t.Fatalf("%s still has %s", s.Name, k)
			}
		}
		if _, has := s.Labels["terminal_type"]; !has {
			t.Fatalf("%s lost terminal_type, which is kept", s.Name)
		}
		if s.Name == "claude_code_token_usage_total" {
			latest[seriesKey(s.Name, s.Labels)] = s.Value // running totals: the last one counts
		}
	}
	var got float64
	for _, v := range latest {
		got += v
	}
	if got != want || want == 0 {
		t.Errorf("tokens after dropping labels: %v, sent: %v", got, want)
	}
}

func TestClaudeCodeSpans(t *testing.T) {
	in := &coltrace.ExportTraceServiceRequest{}
	fixture(t, "traces.json", in)
	red, err := redact.New(config.Redact{})
	if err != nil {
		t.Fatal(err)
	}
	r := &Receiver{Traces: TraceOptions{Sample: 1}, Redact: red.String}
	kept := r.convertTraces(&otlpwire.TraceRequest{ResourceSpans: in.ResourceSpans}, time.Now())
	if kept == nil {
		t.Fatal("no spans kept")
	}

	outputs, subagent := 0, 0
	for _, rs := range kept.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if attr(sp.GetAttributes(), "agent_id") != "" {
					subagent++
				}
				for _, e := range sp.GetEvents() {
					if e.GetName() != "tool.output" {
						continue
					}
					outputs++
					for _, kv := range e.GetAttributes() {
						v := anyString(kv.GetValue())
						if strings.Contains(v, "example-not-a-secret") {
							t.Errorf("the .env password is still in %s: %q", kv.GetKey(), v)
						}
						if attr(e.GetAttributes(), "bash_command") == "cat .env" && kv.GetKey() == "output" && v != "DB_PASSWORD=[redacted]" {
							t.Errorf("output of cat .env: %q", v)
						}
					}
				}
			}
		}
	}
	if outputs != 3 || subagent == 0 {
		t.Errorf("%d tool outputs (want 3), %d subagent spans", outputs, subagent)
	}

	// Claude Code reports its usage as metrics of its own; its spans are
	// not counted again as model calls.
	for _, s := range r.traceMetrics(time.Now()) {
		if strings.HasPrefix(s.Name, "genai_") {
			t.Errorf("%s counted from Claude Code spans: %v", s.Name, s.Labels)
		}
	}
}

// Claude Code sends each counter as a difference, once, when it changed.
// The agent writes the running total again on its clock, so the store
// sees an ordinary counter, and stops an hour after the last report.
func TestDeltaTotalsRepeated(t *testing.T) {
	in := &colmetrics.ExportMetricsServiceRequest{}
	fixture(t, "metrics.json", in)
	r := &Receiver{DropLabels: map[string]bool{"session_id": true}}
	latest := map[uint64]float64{}
	first := map[uint64]int64{}
	for _, s := range r.convertMetrics(&otlpwire.MetricsRequest{ResourceMetrics: in.ResourceMetrics}) {
		k := seriesKey(s.Name, s.Labels)
		if _, seen := latest[k]; !seen {
			// A new counter starts with a zero, before its first total.
			if s.Value != 0 {
				t.Errorf("%s %v starts at %v", s.Name, s.Labels, s.Value)
			}
			first[k] = s.TS
		} else if s.TS <= first[k] {
			t.Errorf("%s: the zero is not before the total", s.Name)
		}
		latest[k] = s.Value
	}
	now := time.Now().Add(15 * time.Second)
	again := r.repeatTotals(now)
	if len(again) != len(latest) {
		t.Fatalf("%d samples repeated, %d series", len(again), len(latest))
	}
	for _, s := range again {
		if v, ok := latest[seriesKey(s.Name, s.Labels)]; !ok || v != s.Value || s.TS != now.UnixMilli() {
			t.Errorf("%s %v: %v at %d, want %v now", s.Name, s.Labels, s.Value, s.TS, v)
		}
	}
	if n := len(r.repeatTotals(time.Now().Add(repeatFor + time.Minute))); n != 0 {
		t.Errorf("%d samples repeated after an hour of silence", n)
	}
}
