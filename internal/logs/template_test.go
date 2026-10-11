package logs

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
)

func compile(t *testing.T, src string) *Template {
	t.Helper()
	var c config.LogTemplate
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	tm, err := Compile("test", c)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestTemplateRegex(t *testing.T) {
	tm := compile(t, `
parse: regex
pattern: '^(?P<client>\S+) \S+ \S+ \[(?P<time>[^\]]+)\] "(?P<method>\S+) (?P<path>\S+) [^"]*" (?P<status>\d+) (?P<bytes>\d+)'
timestamp: {field: time, layout: '02/Jan/2006:15:04:05 -0700'}
labels: [method, status]
drop: ['GET /healthz']
mask:
  - {pattern: 'token=[^ &"]+', replace: 'token=***'}
`)
	line := `10.0.0.1 - - [01/May/2024:10:00:00 +0000] "GET /pay?token=s3cret HTTP/1.1" 500 12`
	msg, ts, labels, fields, ok := tm.Process(line)
	if !ok {
		t.Fatal("dropped")
	}
	if want := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC); !ts.Equal(want) {
		t.Errorf("ts = %v", ts)
	}
	if labels["method"] != "GET" || labels["status"] != "500" || len(labels) != 2 {
		t.Errorf("labels = %v", labels)
	}
	if fields["path"] != "/pay?token=***" || fields["client"] != "10.0.0.1" || fields["bytes"] != "12" {
		t.Errorf("fields = %v", fields)
	}
	if _, kept := fields["time"]; kept || fields["status"] != "" {
		t.Errorf("consumed fields still present: %v", fields)
	}
	if msg != `10.0.0.1 - - [01/May/2024:10:00:00 +0000] "GET /pay?token=*** HTTP/1.1" 500 12` {
		t.Errorf("msg = %q", msg)
	}
	if _, _, _, _, ok := tm.Process(`10.0.0.1 - - [01/May/2024:10:00:00 +0000] "GET /healthz HTTP/1.1" 200 2`); ok {
		t.Error("healthz line not dropped")
	}
	// A line the pattern does not match is kept whole rather than lost.
	if msg, _, _, f, ok := tm.Process("garbage"); !ok || msg != "garbage" || len(f) != 0 {
		t.Errorf("unmatched line: %q %v %v", msg, f, ok)
	}
}

func TestTemplateJSON(t *testing.T) {
	tm := compile(t, `
parse: json
timestamp: {field: ts, layout: unix_ms}
labels: [level]
keep: ['"level":"(warn|error)"']
`)
	msg, ts, labels, fields, ok := tm.Process(`{"ts":1714557600123,"level":"error","msg":"boom","http":{"status":502,"ok":false},"tags":["a","b"],"n":null}`)
	if !ok || msg != "boom" || labels["level"] != "error" || ts.UnixMilli() != 1714557600123 {
		t.Fatalf("msg=%q labels=%v ts=%v ok=%v", msg, labels, ts, ok)
	}
	if fields["http.status"] != "502" || fields["http.ok"] != "false" || fields["tags"] != `["a","b"]` || len(fields) != 3 {
		t.Errorf("fields = %v", fields)
	}
	if _, _, _, _, ok := tm.Process(`{"level":"info","msg":"fine"}`); ok {
		t.Error("info line passed keep filter")
	}
}

func TestTemplateLogfmt(t *testing.T) {
	tm := compile(t, "parse: logfmt\nmessage_field: event\ntimestamp: {field: time}\n")
	msg, ts, _, fields, ok := tm.Process(`time=2024-05-01T10:00:00.5Z level=info event="user \"bob\" logged in" dur=1.5s flag`)
	if !ok || msg != `user "bob" logged in` || ts.IsZero() {
		t.Fatalf("msg=%q ts=%v", msg, ts)
	}
	if fields["level"] != "info" || fields["dur"] != "1.5s" || len(fields) != 3 {
		t.Errorf("fields = %v", fields)
	}
}

func TestCompileErrors(t *testing.T) {
	for _, src := range []string{"parse: xml", "parse: regex", "drop: ['(']", "multiline: {start: '['}"} {
		var c config.LogTemplate
		yaml.Unmarshal([]byte(src), &c)
		if _, err := Compile("bad", c); err == nil {
			t.Errorf("%q: expected error", src)
		}
	}
}
