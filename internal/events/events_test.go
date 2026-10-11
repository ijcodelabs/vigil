package events

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

// record builds a kernel record the way bpf/events.c lays it out.
func record(typ uint32, ts uint64, pid, ppid, uid, euid uint32, comm string, payload []byte) []byte {
	b := make([]byte, hdrSize)
	le := binary.LittleEndian
	le.PutUint64(b[0:], ts)
	le.PutUint64(b[8:], 4242) // cgroup
	le.PutUint32(b[16:], typ)
	le.PutUint32(b[20:], pid)
	le.PutUint32(b[24:], pid)
	le.PutUint32(b[28:], ppid)
	le.PutUint32(b[32:], uid)
	le.PutUint32(b[36:], euid)
	copy(b[40:56], comm)
	return append(b, payload...)
}

func TestDecode(t *testing.T) {
	boot := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	le := binary.LittleEndian

	// exec: args_len, pad, filename[256], args[1024]
	p := make([]byte, 8+256+1024)
	args := "curl\x00-s\x00http://example.com\x00"
	le.PutUint32(p, uint32(len(args)))
	copy(p[8:], "/usr/bin/curl")
	copy(p[264:], args)
	copy(p[264+len(args):], "stale bytes from an earlier record")
	ev, err := decode(record(1, 5e9, 100, 50, 1000, 0, "curl", p), boot)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != TypeExec || ev.PID != 100 || ev.PPID != 50 || ev.UID != 1000 || ev.EUID != 0 || ev.Comm != "curl" ||
		ev.Exe != "/usr/bin/curl" || strings.Join(ev.Args, "|") != "curl|-s|http://example.com" ||
		!ev.Time.Equal(boot.Add(5*time.Second)) || ev.cgroup != 4242 {
		t.Errorf("exec: %+v", ev)
	}

	// connect over IPv4 and listen over IPv6
	n := make([]byte, 8+32)
	le.PutUint16(n, 2)
	le.PutUint16(n[2:], 0)
	le.PutUint16(n[4:], 443)
	copy(n[24:], []byte{93, 184, 216, 34})
	if ev, _ = decode(record(3, 0, 1, 1, 0, 0, "x", n), boot); ev.Addr != "93.184.216.34" || ev.Port != 443 {
		t.Errorf("connect: %+v", ev)
	}
	n = make([]byte, 8+32)
	le.PutUint16(n, 10)
	le.PutUint16(n[2:], 8080)
	if ev, _ = decode(record(4, 0, 1, 1, 0, 0, "x", n), boot); ev.Type != TypeListen || ev.Addr != "::" || ev.Port != 8080 {
		t.Errorf("listen: %+v", ev)
	}

	// open: read-only, then for writing
	o := make([]byte, 8+256)
	copy(o[8:], "/etc/shadow")
	if ev, _ = decode(record(5, 0, 1, 1, 0, 0, "cat", o), boot); ev.Path != "/etc/shadow" || ev.Write {
		t.Errorf("open: %+v", ev)
	}
	le.PutUint32(o, 0x241) // O_WRONLY|O_CREAT|O_TRUNC
	if ev, _ = decode(record(5, 0, 1, 1, 0, 0, "sh", o), boot); !ev.Write {
		t.Errorf("open for write: %+v", ev)
	}

	c := make([]byte, 8)
	le.PutUint32(c, 1000)
	le.PutUint32(c[4:], 0)
	if ev, _ = decode(record(6, 0, 1, 1, 1000, 0, "su", c), boot); *ev.OldEUID != 1000 || *ev.NewEUID != 0 {
		t.Errorf("setuid: %+v", ev)
	}

	x := make([]byte, 8)
	le.PutUint32(x, 3<<8) // exit(3)
	if ev, _ = decode(record(2, 0, 1, 1, 0, 0, "x", x), boot); *ev.ExitCode != 3 || ev.Signal != 0 {
		t.Errorf("exit code: %+v", ev)
	}
	le.PutUint32(x, 9) // killed by SIGKILL
	if ev, _ = decode(record(2, 0, 1, 1, 0, 0, "x", x), boot); *ev.ExitCode != 0 || ev.Signal != 9 {
		t.Errorf("exit signal: %+v", ev)
	}

	for _, bad := range [][]byte{nil, make([]byte, 20), record(1, 0, 1, 1, 0, 0, "x", make([]byte, 10)), record(99, 0, 1, 1, 0, 0, "x", make([]byte, 64))} {
		if _, err := decode(bad, boot); err == nil {
			t.Errorf("decode accepted a %d-byte bad record", len(bad))
		}
	}
}

func security(t *testing.T, rules ...config.SecurityRule) *config.Security {
	t.Helper()
	c, err := config.ParseCollection("")
	if err != nil {
		t.Fatal(err)
	}
	c.Security.Rules = rules
	return &c.Security
}

func TestEngine(t *testing.T) {
	cfg := security(t,
		config.SecurityRule{Name: "shell_in_container", Capture: true, Severity: "critical", MaxPerMinute: 3,
			When:    `type == "exec" && container != "" && exe in ["/bin/sh", "/bin/bash"] && parent_comm != "runc"`,
			Summary: "{exe} started in {container} by {parent_comm} (uid {uid})"},
		config.SecurityRule{Name: "shadow_read", When: `type == "open" && path.startsWith("/etc/shadow") && !(comm in ["sshd", "login"])`},
		config.SecurityRule{Name: "became_root", When: `type == "setuid" && new_euid == 0 && old_euid != 0`},
		config.SecurityRule{Name: "odd_port", When: `type == "connect" && port in [4444, 1337] && args.size() == 0`},
	)
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var dets []Detection
	var caps []*Capture
	e.OnDetection = func(d Detection) { dets = append(dets, d) }
	e.OnCapture = func(c *Capture) { caps = append(caps, c) }

	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }
	zero, thousand := uint32(0), uint32(1000)

	// Background activity before the incident.
	for i := range 100 {
		e.Handle(&Event{Time: at(-60 + float64(i)*0.5), Type: TypeExec, PID: uint32(i), Comm: "cron", Exe: "/usr/sbin/cron"})
	}
	e.Handle(&Event{Time: at(-1), Type: TypeExec, Container: "web", Exe: "/bin/sh", ParentComm: "runc"}) // excluded by the rule
	e.Handle(&Event{Time: at(-0.5), Type: TypeOpen, Path: "/etc/shadow", Comm: "sshd"})                  // allowed reader
	if len(dets) != 0 {
		t.Fatalf("unexpected detections: %+v", dets)
	}

	shell := &Event{Time: at(0), Type: TypeExec, PID: 777, UID: 33, Container: "web", Exe: "/bin/sh", ParentComm: "nginx"}
	e.Handle(shell)
	if len(dets) != 1 || dets[0].Rule != "shell_in_container" || dets[0].Severity != "critical" ||
		dets[0].Summary != "/bin/sh started in web by nginx (uid 33)" || dets[0].CaptureID == "" {
		t.Fatalf("shell detection: %+v", dets)
	}
	capID := dets[0].CaptureID

	// What the intruder does next is detected and tied to the same capture.
	e.Handle(&Event{Time: at(2), Type: TypeOpen, PID: 778, Path: "/etc/shadow", Comm: "cat", Container: "web"})
	e.Handle(&Event{Time: at(3), Type: TypeSetuid, PID: 779, OldEUID: &thousand, NewEUID: &zero, Comm: "exploit"})
	e.Handle(&Event{Time: at(4), Type: TypeConnect, PID: 780, Addr: "203.0.113.9", Port: 4444, Comm: "nc"})
	e.Handle(&Event{Time: at(5), Type: TypeSetuid, OldEUID: &zero, NewEUID: &thousand, Comm: "sshd"}) // dropping privileges is fine
	if len(dets) != 4 || dets[1].Rule != "shadow_read" || dets[1].Severity != "warning" || dets[2].Rule != "became_root" || dets[3].Rule != "odd_port" {
		t.Fatalf("follow-up detections: %+v", dets)
	}
	for _, d := range dets {
		if d.CaptureID != capID {
			t.Errorf("%s not linked to the running capture", d.Rule)
		}
	}
	if d := dets[1]; d.Summary != "open by cat" {
		t.Errorf("default summary: %q", d.Summary)
	}

	// The capture is written once its "after" window (30s) has passed, even
	// if no further event arrives.
	e.Handle(&Event{Time: at(29), Type: TypeExit, PID: 777})
	if len(caps) != 0 {
		t.Fatal("capture finished early")
	}
	e.Tick(at(31))
	if len(caps) != 1 {
		t.Fatal("capture not finished after its window")
	}
	c := caps[0]
	if c.Meta.ID != capID || c.Meta.Rule != "shell_in_container" || c.Trigger != shell || !c.Meta.From.Equal(at(-30)) || !c.Meta.Until.Equal(at(30)) {
		t.Errorf("capture meta: %+v", c.Meta)
	}
	// Background events run from -60s to -10.5s, so 40 of them fall in the
	// window; plus the 2 non-matching ones, the trigger, 4 follow-ups and the exit.
	if c.Meta.Events != 40+2+1+4+1 || c.Events[0].Time.Before(c.Meta.From) || c.Events[len(c.Events)-1].Time.After(c.Meta.Until) {
		t.Errorf("capture holds %d events, first %v", c.Meta.Events, c.Events[0].Time.Sub(t0))
	}
	zr, _ := gzip.NewReader(bytes.NewReader(c.Encode()))
	var back Capture
	if err := json.NewDecoder(zr).Decode(&back); err != nil || back.Meta.ID != capID || len(back.Events) != c.Meta.Events || back.Trigger.PID != 777 {
		t.Errorf("encoded capture does not read back: %v", err)
	}

	// Cooldown: the same rule firing again soon reports, but does not capture.
	dets = nil
	e.Handle(&Event{Time: at(60), Type: TypeExec, Container: "web", Exe: "/bin/bash", ParentComm: "nginx"})
	if len(dets) != 1 || dets[0].CaptureID != "" {
		t.Errorf("capture during cooldown: %+v", dets)
	}
	// Rate limit: at most 3 per minute for this rule.
	for i := range 10 {
		e.Handle(&Event{Time: at(61 + float64(i)), Type: TypeExec, Container: "web", Exe: "/bin/sh", ParentComm: "nginx"})
	}
	if len(dets) != 3 || e.Suppressed != 8 {
		t.Errorf("rate limit: %d reported, %d suppressed", len(dets), e.Suppressed)
	}
	// After the cooldown (5m) it captures again.
	dets = nil
	e.Handle(&Event{Time: at(400), Type: TypeExec, Container: "web", Exe: "/bin/sh", ParentComm: "nginx"})
	if len(dets) != 1 || dets[0].CaptureID == "" || dets[0].CaptureID == capID {
		t.Errorf("after cooldown: %+v", dets)
	}
}

func TestRecorderTrims(t *testing.T) {
	r := recorder{window: 10 * time.Second}
	t0 := time.Now()
	last := t0
	for i := range 100000 { // 100 seconds at 1000 events/s
		last = t0.Add(time.Duration(i) * time.Millisecond)
		r.add(&Event{Time: last})
	}
	// Everything inside the window is there...
	if got := r.between(last.Add(-10*time.Second), last); len(got) != 10001 {
		t.Errorf("window holds %d events, want 10001", len(got))
	}
	// ...and memory stays near the window instead of growing with history.
	if len(r.evs) > 25000 {
		t.Errorf("recorder holds %d events for a 10s window at 1000/s", len(r.evs))
	}
}

func TestValidate(t *testing.T) {
	for want, r := range map[string]config.SecurityRule{
		"name must be":           {Name: "bad name", When: "true"},
		"severity must be":       {Name: "r", When: "true", Severity: "fatal"},
		"undeclared reference":   {Name: "r", When: `typ == "exec"`},
		"must be true or false":  {Name: "r", When: `comm`},
		"not an event field":     {Name: "r", When: "true", Summary: "{exe} {nope}"},
		"Syntax error":           {Name: "r", When: `type == `},
		"found no matching over": {Name: "r", When: `port == "80"`},
	} {
		if err := Validate(security(t, r)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want error containing %q", r, err, want)
		}
	}
	if err := Validate(security(t, config.SecurityRule{Name: "a", When: "true"}, config.SecurityRule{Name: "a", When: "true"})); err == nil {
		t.Error("duplicate rule names accepted")
	}
	c := security(t)
	c.WatchPaths = []string{"etc/shadow"}
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative watch path: %v", err)
	}
	c.WatchPaths = make([]string, 17)
	if err := Validate(c); err == nil {
		t.Error("17 watch paths accepted")
	}
}
