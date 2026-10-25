// Package claudehook turns Claude Code hook calls into log lines for the
// agent: an audit trail of what ran, which permissions were asked for and
// refused, and which subagents were started, by whom and where.
//
// Claude Code's own telemetry counts and prices; it does not say who ran
// which command in which folder unless every content setting is on. A
// hook sees exactly that, one call at a time, so `vigil-agent claude-hook` is
// registered as a command hook and sends one OTLP log record per call to
// the agent on the same machine.
//
// A hook must never get in the way: on UserPromptSubmit and SessionStart
// whatever it prints is given to the model, and exit code 2 blocks the
// prompt. So nothing is printed and the command always exits 0.
package claudehook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// Service is the service name of the records; the dashboards and the
// Sessions page match service:~"^claude-code".
const Service = "claude-code-hook"

// maxText bounds every value taken from the input.
const maxText = 2000

// Options say what goes beyond the facts of a call.
type Options struct {
	// Content adds the text of prompts and the instructions given to
	// subagents. Off by default: everyone who can read vigil can read it.
	Content bool
	User    string // the operating-system account Claude Code runs as
}

// input is what Claude Code passes on stdin, as far as it is kept.
type input struct {
	SessionID      string          `json:"session_id"`
	PromptID       string          `json:"prompt_id"`
	Event          string          `json:"hook_event_name"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	AgentID        string          `json:"agent_id"`
	AgentType      string          `json:"agent_type"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      map[string]any  `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	McpServer      string          `json:"mcp_server"`
	DurationMs     *float64        `json:"duration_ms"`
	Error          any             `json:"error"`
	IsInterrupt    *bool           `json:"is_interrupt"`
	Reason         string          `json:"reason"`
	Source         string          `json:"source"`
	Model          string          `json:"model"`
	Prompt         *string         `json:"prompt"`
}

// toolFields are the parts of a tool's input worth an audit line, by the
// names Claude Code's tools use. Contents (a file's new text, a patch)
// are left out; their size is kept.
var toolFields = []string{"command", "description", "file_path", "path", "pattern", "glob", "url", "query", "subagent_type", "notebook_path", "skill"}

// Record turns one hook input into an OTLP log request.
func Record(raw []byte, now time.Time, o Options) (*collogs.ExportLogsServiceRequest, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("hook input: %w", err)
	}
	if in.Event == "" {
		return nil, fmt.Errorf("hook input without hook_event_name")
	}
	var attrs []*commonpb.KeyValue
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, str(k, cut(v)))
		}
	}
	add("session.id", in.SessionID)
	add("prompt.id", in.PromptID)
	add("hook_event", in.Event)
	add("cwd", in.Cwd)
	add("permission_mode", in.PermissionMode)
	add("agent_id", in.AgentID)
	add("agent_type", in.AgentType)
	add("os_user", o.User)
	add("tool_name", in.ToolName)
	add("tool_use_id", in.ToolUseID)
	add("mcp_server", in.McpServer)
	add("reason", in.Reason)
	add("start_source", in.Source) // "source" is a stream label in vigil
	add("model", in.Model)
	if in.DurationMs != nil {
		add("duration_ms", strconv.FormatFloat(*in.DurationMs, 'f', -1, 64))
	}
	if in.IsInterrupt != nil {
		add("is_interrupt", strconv.FormatBool(*in.IsInterrupt))
	}
	switch e := in.Error.(type) {
	case nil:
	case string:
		add("error", e)
	default:
		b, _ := json.Marshal(e)
		add("error", string(b))
	}
	for _, k := range toolFields {
		if v, ok := in.ToolInput[k]; ok {
			add("tool."+k, text(v))
		}
	}
	if len(in.ToolInput) > 0 {
		b, _ := json.Marshal(in.ToolInput)
		add("tool.input_bytes", strconv.Itoa(len(b)))
		if p, ok := in.ToolInput["prompt"]; ok && o.Content {
			add("tool.prompt", text(p)) // a subagent's instructions
		}
	}
	if len(in.ToolResponse) > 0 {
		add("tool.response_bytes", strconv.Itoa(len(in.ToolResponse)))
	}
	if in.Prompt != nil {
		add("prompt_length", strconv.Itoa(utf8.RuneCountInString(*in.Prompt)))
		if o.Content {
			add("prompt", *in.Prompt)
		}
	}
	severity := logspb.SeverityNumber_SEVERITY_NUMBER_INFO
	if in.Event == "PermissionDenied" || in.Event == "PostToolUseFailure" {
		severity = logspb.SeverityNumber_SEVERITY_NUMBER_WARN
	}
	ts := uint64(now.UnixNano())
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", Service)}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: ts, ObservedTimeUnixNano: ts, SeverityNumber: severity,
			Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.hook." + in.Event}},
			Attributes: attrs,
		}}}},
	}}}, nil
}

// Send posts the request to an agent's OTLP endpoint.
func Send(ctx context.Context, endpoint string, req *collogs.ExportLogsServiceRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("agent answered %s", resp.Status)
	}
	return nil
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func cut(s string) string {
	if utf8.RuneCountInString(s) <= maxText {
		return s
	}
	return string([]rune(s)[:maxText]) + "…"
}
