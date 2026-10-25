package logs

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ijcodelabs/vigil/internal/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/model"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// collector drains records the way the manager does, applying positions.
type collector struct {
	t    *testing.T
	recs chan Record
	pos  *Positions
}

func newCollector(t *testing.T) *collector {
	pos, err := LoadPositions(filepath.Join(t.TempDir(), "positions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &collector{t: t, recs: make(chan Record, 1024), pos: pos}
}

// next returns the next n entries, failing if they do not arrive.
func (c *collector) next(n int) []model.LogEntry {
	c.t.Helper()
	var out []model.LogEntry
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case r := <-c.recs:
			if r.Delete {
				c.pos.Delete(r.Key)
			} else if r.Key != "" && r.Pos != "" {
				c.pos.Set(r.Key, r.Pos)
			}
			if r.Entry != nil {
				out = append(out, *r.Entry)
			}
		case <-deadline:
			c.t.Fatalf("got %d of %d entries: %+v", len(out), n, out)
		}
	}
	return out
}

// none fails if an entry arrives within d; position-only records are applied.
func (c *collector) none(d time.Duration) {
	c.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case r := <-c.recs:
			if r.Entry != nil {
				c.t.Fatalf("unexpected entry: %+v", *r.Entry)
			}
			if r.Delete {
				c.pos.Delete(r.Key)
			} else if r.Key != "" && r.Pos != "" {
				c.pos.Set(r.Key, r.Pos)
			}
		case <-timeout:
			return
		}
	}
}

func msgs(es []model.LogEntry) string {
	var s []string
	for _, e := range es {
		s = append(s, e.Msg)
	}
	return strings.Join(s, "|")
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

func TestTailer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	appendFile(t, path, "history\n")
	appendFile(t, filepath.Join(dir, "app.log.gz"), "compressed\n")

	c := newCollector(t)
	start := func() context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		tl := &Tailer{Pos: c.pos, Out: c.recs, Log: quiet, Poll: 20 * time.Millisecond, Sources: []FileSource{
			{Paths: []string{path}, Template: &Template{}, Labels: map[string]string{"app": "demo"}},
			{Paths: []string{filepath.Join(dir, "*.gz")}, Template: &Template{}, FromBeginning: true}}}
		done := make(chan struct{})
		go func() { tl.Run(ctx); close(done) }()
		return func() { cancel(); <-done }
	}
	stop := start()

	c.none(150 * time.Millisecond) // history at startup and compressed files are not shipped
	appendFile(t, path, "a\nb\r\n")
	got := c.next(2)
	if msgs(got) != "a|b" || got[0].Stream["app"] != "demo" || got[0].Stream["path"] != path || got[0].Stream["source"] != "file" {
		t.Fatalf("got %+v", got)
	}

	appendFile(t, path, "c") // a line still being written
	c.none(150 * time.Millisecond)
	appendFile(t, path, "d\n")
	if got := c.next(1); msgs(got) != "cd" {
		t.Fatalf("partial line: %q", msgs(got))
	}

	// Rotation by rename: the last line of the old file must not be lost.
	appendFile(t, path, "last\n")
	os.Rename(path, filepath.Join(dir, "app.log.1"))
	appendFile(t, path, "new1\n")
	if got := c.next(2); msgs(got) != "last|new1" {
		t.Fatalf("rotation: %q", msgs(got))
	}

	// Truncation in place.
	os.Truncate(path, 0)
	appendFile(t, path, "t\n")
	if got := c.next(1); msgs(got) != "t" {
		t.Fatalf("truncate: %q", msgs(got))
	}

	// Restart: lines written while the agent was down are shipped, nothing twice.
	stop()
	appendFile(t, path, "while-down\n")
	stop = start()
	defer stop()
	if got := c.next(1); msgs(got) != "while-down" {
		t.Fatalf("restart: %q", msgs(got))
	}
	c.none(150 * time.Millisecond)
}

func TestTailerMultiline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "java.log")
	c := newCollector(t)
	tm := compile(t, "multiline: {start: '^\\d{4}-'}\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tl := &Tailer{Pos: c.pos, Out: c.recs, Log: quiet, Poll: 20 * time.Millisecond,
		Sources: []FileSource{{Paths: []string{path}, Template: tm, FromBeginning: true}}}
	appendFile(t, path, "2024-05-01 ERROR boom\n  at a.b(C.java)\n  at d.e(F.java)\n2024-05-01 INFO fine\n")
	go tl.Run(ctx)
	got := c.next(2)
	if got[0].Msg != "2024-05-01 ERROR boom\n  at a.b(C.java)\n  at d.e(F.java)" || got[1].Msg != "2024-05-01 INFO fine" {
		t.Fatalf("got %q", msgs(got))
	}
}

func TestTailerKubernetes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "prod_api-7d9_0a1b", "app")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "0.log")
	appendFile(t, path, "")
	c := newCollector(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tl := &Tailer{Pos: c.pos, Out: c.recs, Log: quiet, Poll: 20 * time.Millisecond, Sources: []FileSource{{
		Paths: []string{filepath.Join(root, "*", "*", "*.log")}, Format: formatCRI, Template: &Template{},
		PerFile: func(p string) (map[string]string, *Template, bool) { l, ok := podLogLabels(p); return l, nil, ok },
	}}}
	go tl.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	appendFile(t, path, "2024-05-01T10:00:00.5Z stdout P first half, \n2024-05-01T10:00:00.6Z stdout F second half\n2024-05-01T10:00:01Z stderr F oops\n")
	got := c.next(2)
	e := got[0]
	if e.Msg != "first half, second half" || e.Fields["stream"] != "stdout" || got[1].Fields["stream"] != "stderr" {
		t.Fatalf("got %+v", got)
	}
	if e.Stream["namespace"] != "prod" || e.Stream["pod"] != "api-7d9" || e.Stream["container"] != "app" || e.Stream["source"] != "kubernetes" {
		t.Errorf("stream = %v", e.Stream)
	}
	if want := time.Date(2024, 5, 1, 10, 0, 0, 600e6, time.UTC); e.Time != want.UnixNano() {
		t.Errorf("time = %v", time.Unix(0, e.Time).UTC())
	}
}

const fakeJournal = `{"MESSAGE":"Started nginx.","__CURSOR":"c1","__REALTIME_TIMESTAMP":"1714557600000000","_SYSTEMD_UNIT":"nginx.service","PRIORITY":"6","SYSLOG_IDENTIFIER":"systemd","_PID":"1","_TRANSPORT":"journal"}
{"MESSAGE":[99,97,102,195,169,27,91,48,109],"__CURSOR":"c2","_SYSTEMD_USER_UNIT":"sync.service"}
{"MESSAGE":"disk failing","__CURSOR":"c3","PRIORITY":"3","_TRANSPORT":"kernel","SYSLOG_IDENTIFIER":"kernel"}
{"MESSAGE":"2026-10-10 12:00:00 ERROR boom","__CURSOR":"c4","_SYSTEMD_UNIT":"shop.service","_TRANSPORT":"stdout"}
{"MESSAGE":"session opened","__CURSOR":"c5","_SYSTEMD_UNIT":"ssh.service","SYSLOG_IDENTIFIER":"sshd"}
{"MESSAGE":"  at Shop.pay(Shop.java:42)","__CURSOR":"c6","_SYSTEMD_UNIT":"shop.service"}
{"MESSAGE":"  at Main.run(Main.java:7)","__CURSOR":"c7","_SYSTEMD_UNIT":"shop.service"}
{"MESSAGE":"2026-10-10 12:00:01 INFO recovered","__CURSOR":"c8","_SYSTEMD_UNIT":"shop.service"}
{"MESSAGE":"password check failed","__CURSOR":"c9","SYSLOG_IDENTIFIER":"unix_chkpwd","_TRANSPORT":"syslog"}
`

func TestJournald(t *testing.T) {
	c := newCollector(t)
	journal := filepath.Join(t.TempDir(), "journal.json")
	os.WriteFile(journal, []byte(fakeJournal), 0o644)
	var mu sync.Mutex
	var seen [][]string
	java, err := Compile("java", config.LogTemplate{Multiline: struct {
		Start string `yaml:"start"`
	}{Start: `^\d{4}-\d{2}-\d{2}`}, Add: map[string]string{"app": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	j := &Journald{Template: &Template{}, Templates: map[string]*Template{"java": java}, ByUnit: map[string]string{"shop*": "java"},
		Pos: c.pos, Out: c.recs, Log: quiet,
		Command: func(ctx context.Context, args []string) *exec.Cmd {
			mu.Lock()
			seen = append(seen, args)
			mu.Unlock()
			return exec.CommandContext(ctx, "sh", "-c", "cat "+journal+"; sleep 60")
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go j.Run(ctx)
	by := map[string]*model.LogEntry{}
	for _, e := range c.next(7) {
		by[strings.SplitN(e.Msg, "\n", 2)[0]] = &e
	}
	e := by["Started nginx."]
	if e == nil || e.Stream["unit"] != "nginx.service" || e.Stream["source"] != "journald" || e.Fields["level"] != "info" ||
		e.Fields["identifier"] != "systemd" || e.Fields["transport"] != "journal" || e.Time != 1714557600000000*1000 {
		t.Errorf("unit entry: %+v", e)
	}
	// A message journalctl printed as bytes is text again; a user's unit counts as the unit.
	if e := by["caf\u00e9\x1b[0m"]; e == nil || e.Stream["unit"] != "sync.service" {
		t.Errorf("byte-array message: %v", by)
	}
	// What the kernel says has no unit of its own.
	if e := by["disk failing"]; e == nil || e.Stream["unit"] != "kernel" || e.Fields["level"] != "error" {
		t.Errorf("kernel entry: %+v", e)
	}
	// Nothing to name it by: no unit label, the identifier is still there.
	if e := by["password check failed"]; e == nil || e.Stream["unit"] != "" || e.Fields["identifier"] != "unix_chkpwd" {
		t.Errorf("entry without a unit: %+v", e)
	}
	// The stack trace is joined although another unit logged in between,
	// and that unit's line is left alone.
	e = by["2026-10-10 12:00:00 ERROR boom"]
	if e == nil || e.Msg != "2026-10-10 12:00:00 ERROR boom\n  at Shop.pay(Shop.java:42)\n  at Main.run(Main.java:7)" || e.Stream["app"] != "shop" {
		t.Errorf("joined entry: %+v", e)
	}
	if e := by["session opened"]; e == nil || e.Stream["unit"] != "ssh.service" || e.Stream["app"] != "" {
		t.Errorf("the other unit: %+v", e)
	}
	if e := by["2026-10-10 12:00:01 INFO recovered"]; e == nil { // sent once the unit went quiet
		t.Errorf("last entry of the multi-line unit missing: %v", by)
	}
	if p, _ := c.pos.Get(journaldKey); p != "c9" {
		t.Errorf("cursor = %q", p)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(seen[0], " "), "--lines=0") {
		t.Errorf("first run args: %v", seen[0])
	}
}

// frame builds one Docker multiplexed-stream frame.
func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDocker(t *testing.T) {
	var mu sync.Mutex
	since := map[string]string{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			io.WriteString(w, `[
			 {"Id":"aaa111","Names":["/web"],"Image":"nginx:1","State":"running","Labels":{"vigil.logs.template":"kv"}},
			 {"Id":"bbb222","Names":["/quiet"],"Image":"x","State":"running","Labels":{"vigil.logs":"false"}},
			 {"Id":"ccc333","Names":["/stopped"],"Image":"x","State":"exited","Labels":{}}]`)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":2500000000}},
			 "memory_stats":{"usage":1000,"limit":4000,"stats":{"inactive_file":300}},
			 "networks":{"eth0":{"rx_bytes":10,"tx_bytes":20}}}`)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			id := strings.Split(r.URL.Path, "/")[2]
			mu.Lock()
			since[id] = r.URL.Query().Get("since")
			mu.Unlock()
			w.Write(frame(1, "2024-05-01T10:00:00.000000001Z level=info msg=hello\n"))
			// One line split over two frames, interleaved with stderr.
			w.Write(frame(1, "2024-05-01T10:00:00.000000002Z level=warn "))
			w.Write(frame(2, "2024-05-01T10:00:00.000000003Z plain stderr line\n"))
			w.Write(frame(1, "msg=split\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/json"):
			running := !strings.Contains(r.URL.Path, "ccc333")
			fmt.Fprintf(w, `{"Config":{"Tty":false},"State":{"Running":%v,"ExitCode":0}}`, running)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client := dockerapi.NewHTTP(api.URL)

	c := newCollector(t)
	c.pos.Set(dockerKeyPrefix+"aaa111", "1714557600000000000")
	c.pos.Set(dockerKeyPrefix+"removed", "1")
	kv := compile(t, "parse: logfmt\nlabels: [level]\n")
	d := &Docker{Client: client, Default: &Template{}, Templates: map[string]*Template{"kv": kv},
		Labels: map[string]string{"env": "test"}, Pos: c.pos, Out: c.recs, Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	got := c.next(3)
	byMsg := map[string]model.LogEntry{}
	for _, e := range got {
		byMsg[e.Msg] = e
	}
	hello, split := byMsg["hello"], byMsg["split"]
	if hello.Stream["container"] != "web" || hello.Stream["image"] != "nginx:1" || hello.Stream["level"] != "info" ||
		hello.Stream["env"] != "test" || hello.Fields["stream"] != "stdout" {
		t.Errorf("hello: %+v", hello)
	}
	if split.Stream["level"] != "warn" {
		t.Errorf("split line: %+v (all: %q)", split, msgs(got))
	}
	if e, ok := byMsg["plain stderr line"]; !ok || e.Fields["stream"] != "stderr" {
		t.Errorf("stderr line missing: %q", msgs(got))
	}
	c.none(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if since["aaa111"] != "1714557600.000000001" {
		t.Errorf("resume since = %q", since["aaa111"])
	}
	if _, streamed := since["bbb222"]; streamed {
		t.Error("opted-out container was streamed")
	}
	if _, ok := c.pos.Get(dockerKeyPrefix + "removed"); ok {
		t.Error("position of a removed container was kept")
	}

	samples, err := client.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]float64{}
	for _, s := range samples {
		vals[fmt.Sprintf("%s/%s", s.Labels["container"], s.Name)] = s.Value
	}
	if vals["web/container_cpu_usage_seconds_total"] != 2.5 || vals["web/container_memory_usage_bytes"] != 700 ||
		vals["web/container_network_transmit_bytes_total"] != 20 || vals["web/container_running"] != 1 ||
		vals["stopped/container_running"] != 0 || len(samples) != 17 {
		t.Errorf("metrics: %v", vals)
	}
}

// A stream that stops delivering is closed and reopened from where it left
// off, losing and repeating nothing; a container that never writes is
// reopened from when it was first seen, not from its first line.
func TestDockerStalledStreamIsReopened(t *testing.T) {
	var mu sync.Mutex
	calls := map[string][]string{} // container → since of each logs call
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			io.WriteString(w, `[{"Id":"stall1","Names":["/web"],"Image":"x","State":"running","Labels":{}},
			 {"Id":"quiet1","Names":["/quiet"],"Image":"x","State":"running","Labels":{}}]`)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			id := strings.Split(r.URL.Path, "/")[2]
			mu.Lock()
			calls[id] = append(calls[id], r.URL.Query().Get("since"))
			n := len(calls[id])
			mu.Unlock()
			if id == "stall1" {
				if n == 1 { // one line, then silence: Docker stopped sending
					w.Write(frame(1, "2024-05-01T10:00:00.000000001Z first\n"))
				} else { // reopened: what came meanwhile
					w.Write(frame(1, "2024-05-01T10:00:05.000000000Z second\n"))
				}
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Config":{"Tty":false},"State":{"Running":true,"ExitCode":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	c := newCollector(t)
	c.pos.Set(dockerKeyPrefix+"stall1", "1714557599000000000")
	d := &Docker{Client: dockerapi.NewHTTP(api.URL), Default: &Template{}, Pos: c.pos, Out: c.recs, Log: quiet,
		Idle: 300 * time.Millisecond, Rescan: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	got := c.next(2)
	if m := msgs(got); m != "first|second" {
		t.Fatalf("lines: %q", m)
	}
	c.none(200 * time.Millisecond) // nothing repeated
	mu.Lock()
	defer mu.Unlock()
	if s := calls["stall1"]; len(s) < 2 || s[0] != "1714557599.000000001" || s[1] != "1714557600.000000002" {
		t.Errorf("stalled stream: reopened from %q (want after the last line read)", s)
	}
	q := calls["quiet1"]
	if len(q) < 2 || q[0] == "0" || q[1] != q[0]+".000000000" {
		t.Errorf("quiet container: opened with since %q (want the first look's time again, never 0)", q)
	}
}
