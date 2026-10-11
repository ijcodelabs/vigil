package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const journaldKey = "journald"

var priorities = []string{"emerg", "alert", "crit", "error", "warning", "notice", "info", "debug"}

// Journald follows the systemd journal by running journalctl, which keeps
// the agent a static binary (the journal library needs cgo and libsystemd).
type Journald struct {
	Units    []string
	Template *Template
	// Templates and ByUnit choose another template for some units; see
	// config's logs.journald.unit_templates.
	Templates map[string]*Template
	ByUnit    map[string]string
	Labels    map[string]string
	Pos       *Positions
	Out       chan<- Record
	Log       *slog.Logger

	// Command builds the journalctl process; replaceable in tests.
	Command func(ctx context.Context, args []string) *exec.Cmd
}

func (j *Journald) Run(ctx context.Context) {
	if j.Command == nil {
		if _, err := exec.LookPath("journalctl"); err != nil {
			j.Log.Warn("journald logs disabled: journalctl not found")
			return
		}
		j.Command = func(ctx context.Context, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, "journalctl", args...)
		}
	}
	stream := map[string]string{"source": "journald"}
	for k, v := range j.Labels {
		stream[k] = v
	}
	s := &sink{ctx: ctx, out: j.Out, key: journaldKey, tmpl: j.Template, stream: stream}
	cursor, _ := j.Pos.Get(journaldKey)
	for ctx.Err() == nil {
		var err error
		if cursor, err = j.follow(ctx, s, cursor); err != nil && ctx.Err() == nil {
			j.Log.Warn("journalctl stopped, restarting", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// follow runs journalctl once and returns the last cursor it saw.
func (j *Journald) follow(ctx context.Context, s *sink, cursor string) (string, error) {
	args := []string{"--output=json", "--follow", "--no-pager"}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "--lines=0") // first run: only what comes from now on
	}
	for _, u := range j.Units {
		args = append(args, "--unit="+u)
	}
	cmd := j.Command(ctx, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cursor, err
	}
	if err := cmd.Start(); err != nil {
		return cursor, err
	}
	lines := make(chan block, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			if b, ok := parseJournal(sc.Bytes()); ok {
				select {
				case lines <- b:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	if last := j.pump(s, lines); last != "" {
		cursor = last
	}
	if ctx.Err() != nil {
		return cursor, nil
	}
	return cursor, cmd.Wait()
}

// maxJournalUnits bounds how many units are tracked for joining lines.
const maxJournalUnits = 4096

// pump is the journal's own: entries of all units arrive interleaved, so
// lines are joined into multi-line entries per unit, each with the template
// chosen for that unit. An entry still being joined is sent when its unit
// has been quiet for a second.
func (j *Journald) pump(s *sink, lines <-chan block) (last string) {
	type unit struct {
		asm  assembler
		sink *sink
	}
	units := map[string]*unit{}
	var flush func() bool
	get := func(b block) *unit {
		name := b.labels["unit"]
		if u := units[name]; u != nil {
			return u
		}
		tmpl := s.tmpl
		if t := j.Templates[matchTemplate(j.ByUnit, name, b.fields["identifier"])]; t != nil {
			tmpl = t
		}
		if len(units) >= maxJournalUnits { // transient units come and go; start over rather than grow
			flush()
			units = map[string]*unit{}
		}
		us := *s
		us.tmpl = tmpl
		u := &unit{asm: assembler{start: tmpl.start}, sink: &us}
		units[name] = u
		return u
	}
	// An entry that waited for more lines is sent after later entries of
	// other units. It must not move the saved position back to its own
	// cursor, or a restart would read those again: it carries the newest.
	emit := func(u *unit, done block) bool {
		done.pos = last
		return u.sink.emit(done)
	}
	flush = func() bool {
		for _, u := range units {
			if done, ok := u.asm.flush(); ok && !emit(u, done) {
				return false
			}
		}
		return true
	}
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return last
		case b, ok := <-lines:
			if !ok {
				flush()
				return last
			}
			if b.pos != "" {
				last = b.pos
			}
			u := get(b)
			for _, done := range u.asm.add(b) {
				if !emit(u, done) {
					return last
				}
			}
		case <-idle.C:
			if !flush() {
				return last
			}
		}
	}
}

// journalText reads a journal field. journalctl prints a value that is not
// plain text (control characters, another encoding) as a list of bytes.
func journalText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		b := make([]byte, 0, len(x))
		for _, e := range x {
			n, ok := e.(float64)
			if !ok || n < 0 || n > 255 {
				return ""
			}
			b = append(b, byte(n))
		}
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	return ""
}

func parseJournal(line []byte) (block, bool) {
	var raw map[string]any
	if json.Unmarshal(line, &raw) != nil {
		return block{}, false
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	b := block{text: journalText(raw["MESSAGE"]), pos: str("__CURSOR"), fields: map[string]string{}, labels: map[string]string{}}
	if b.text == "" || b.pos == "" {
		return b, false
	}
	if us, err := strconv.ParseInt(str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		b.time = time.UnixMicro(us)
	}
	// Every line should say where it is from. Not everything in the journal
	// belongs to a system unit: a user's service has a user unit, and what
	// the kernel says (firewall blocks, hardware, the OOM killer) has none.
	transport := str("_TRANSPORT")
	switch {
	case str("_SYSTEMD_UNIT") != "":
		b.labels["unit"] = str("_SYSTEMD_UNIT")
	case str("_SYSTEMD_USER_UNIT") != "":
		b.labels["unit"] = str("_SYSTEMD_USER_UNIT")
	case transport == "kernel":
		b.labels["unit"] = "kernel"
	case transport == "audit":
		b.labels["unit"] = "audit"
	}
	if transport != "" {
		b.fields["transport"] = transport
	}
	if p, err := strconv.Atoi(str("PRIORITY")); err == nil && p >= 0 && p < len(priorities) {
		b.fields["level"] = priorities[p]
	}
	for field, name := range map[string]string{"SYSLOG_IDENTIFIER": "identifier", "_PID": "pid"} {
		if v := str(field); v != "" {
			b.fields[name] = v
		}
	}
	return b, true
}
