package logs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	maxLineBytes  = 256 << 10
	readBudget    = 4 << 20 // per file per poll, so one busy file cannot starve the rest
	rescanEvery   = 10 * time.Second
	formatCRI     = "cri"
	defaultPoll   = 500 * time.Millisecond
	fileKeyPrefix = "file:"
)

var compressedSuffixes = []string{".gz", ".bz2", ".xz", ".zst", ".zip"}

// FileSource is a set of globs tailed with one template.
type FileSource struct {
	Paths         []string
	Exclude       []string
	Format        string // "" for plain lines, "cri" for Kubernetes pod logs
	Template      *Template
	Labels        map[string]string
	FromBeginning bool
	// PerFile, if set, supplies extra stream labels and a template override
	// derived from the path. Returning ok=false skips the file.
	PerFile func(path string) (labels map[string]string, tmpl *Template, ok bool)
}

// Tailer follows files by polling. Polling needs no kernel support, works on
// network filesystems, and is cheap for the number of files a host has.
type Tailer struct {
	Sources []FileSource
	Pos     *Positions
	Out     chan<- Record
	Log     *slog.Logger
	Poll    time.Duration

	ctx   context.Context
	files map[string]*tailed
}

type tailed struct {
	path    string
	f       *os.File
	ino     uint64
	offset  int64 // bytes read from the file so far
	partial []byte
	format  string
	criBuf  string // fragments of a CRI partial line
	asm     assembler
	sink    sink
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func (t *Tailer) Run(ctx context.Context) {
	t.ctx, t.files = ctx, map[string]*tailed{}
	if t.Poll == 0 {
		t.Poll = defaultPoll
	}
	t.scan(true)
	lastScan := time.Now()
	tick := time.NewTicker(t.Poll)
	defer tick.Stop()
	for {
		for _, f := range t.files {
			t.read(f)
		}
		select {
		case <-ctx.Done():
			for _, f := range t.files {
				f.f.Close()
			}
			return
		case <-tick.C:
		}
		if time.Since(lastScan) >= rescanEvery {
			t.scan(false)
			lastScan = time.Now()
		}
	}
}

// scan opens files that match a glob and are not followed yet.
func (t *Tailer) scan(initial bool) {
	for i := range t.Sources {
		src := &t.Sources[i]
		for _, pattern := range src.Paths {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				t.Log.Warn("bad log path pattern", "pattern", pattern, "err", err)
				continue
			}
			for _, path := range matches {
				if _, ok := t.files[path]; ok || excluded(path, src.Exclude) {
					continue
				}
				if err := t.open(path, src, initial); err != nil {
					t.Log.Warn("cannot follow log file", "path", path, "err", err)
				}
			}
		}
	}
}

func excluded(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, s := range compressedSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

// open starts following path. Where it starts reading:
//   - a remembered position for the same file: there;
//   - a file already present when the agent starts, never seen before: at
//     the end (unless from_beginning), so old history is not re-shipped;
//   - anything else (new or rotated file): at the beginning.
func (t *Tailer) open(path string, src *FileSource, initial bool) error {
	labels, tmpl := map[string]string{"source": "file", "path": path}, src.Template
	if src.PerFile != nil {
		l, tm, ok := src.PerFile(path)
		if !ok {
			return nil
		}
		labels = l
		if tm != nil {
			tmpl = tm
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return err
	}
	for k, v := range src.Labels {
		labels[k] = v
	}
	tf := &tailed{path: path, f: f, ino: inode(fi), format: src.Format,
		asm:  assembler{start: tmpl.start},
		sink: sink{ctx: t.ctx, out: t.Out, key: fileKeyPrefix + path, tmpl: tmpl, stream: labels}}

	var ino uint64
	var off int64
	if saved, ok := t.Pos.Get(tf.sink.key); ok {
		if _, err := fmt.Sscanf(saved, "%d:%d", &ino, &off); err == nil && ino == tf.ino && off <= fi.Size() {
			tf.offset = off
		}
	} else if initial && !src.FromBeginning {
		tf.offset = fi.Size()
	}
	if _, err := f.Seek(tf.offset, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	t.files[path] = tf
	return nil
}

// read drains new data, then checks whether the path was rotated, truncated
// or removed. The old file stays open until drained, so lines written just
// before a rotation are not lost.
func (t *Tailer) read(f *tailed) {
	buf := make([]byte, 64<<10)
	got, eof := 0, false
	for got < readBudget {
		n, err := f.f.Read(buf)
		if n > 0 {
			got += n
			if !f.consume(buf[:n]) {
				return
			}
		}
		if err != nil {
			eof = true
			break
		}
	}
	if !eof {
		return
	}
	if got == 0 { // quiet: whatever multi-line entry is pending is complete
		if b, ok := f.asm.flush(); ok && !f.sink.emit(b) {
			return
		}
	}
	fi, err := os.Stat(f.path)
	switch {
	case err != nil:
		f.finish()
		f.f.Close()
		delete(t.files, f.path)
		f.sink.send(Record{Key: f.sink.key, Delete: true})
	case inode(fi) != f.ino:
		f.finish()
		f.f.Close()
		delete(t.files, f.path)
		t.scan(false) // picks the new file up from its beginning
	case fi.Size() < f.offset: // truncated in place (copytruncate)
		f.f.Seek(0, io.SeekStart)
		f.offset, f.partial, f.criBuf = 0, nil, ""
		f.asm.flush()
	}
}

// finish emits what is still buffered when a file ends for good.
func (f *tailed) finish() {
	if len(f.partial) > 0 {
		f.line(string(f.partial), f.offset)
		f.partial = nil
	}
	if b, ok := f.asm.flush(); ok {
		f.sink.emit(b)
	}
}

func (f *tailed) consume(b []byte) bool {
	data := append(f.partial, b...)
	base := f.offset - int64(len(f.partial)) // file offset of data[0]
	f.offset += int64(len(b))
	pos := 0
	for {
		i := bytes.IndexByte(data[pos:], '\n')
		if i < 0 {
			break
		}
		if !f.line(string(data[pos:pos+i]), base+int64(pos+i+1)) {
			return false
		}
		pos += i + 1
	}
	rest := data[pos:]
	if len(rest) > maxLineBytes { // never buffer an endless line
		if !f.line(string(rest), f.offset) {
			return false
		}
		rest = nil
	}
	f.partial = append([]byte(nil), rest...)
	return true
}

// line handles one physical line ending at file offset end.
func (f *tailed) line(text string, end int64) bool {
	b := block{text: strings.TrimSuffix(text, "\r"), pos: fmt.Sprintf("%d:%d", f.ino, end)}
	if f.format == formatCRI {
		ts, stream, full, msg, ok := parseCRI(b.text)
		if ok {
			if !full {
				f.criBuf += msg
				return true
			}
			b.text, b.time, b.fields = f.criBuf+msg, ts, map[string]string{"stream": stream}
			f.criBuf = ""
		}
	}
	for _, done := range f.asm.add(b) {
		if !f.sink.emit(done) {
			return false
		}
	}
	return true
}

// parseCRI splits a container runtime log line:
//
//	2024-05-01T10:00:00.123456789Z stdout F the message
//
// full is false for a "P" line, a fragment of a longer message.
func parseCRI(line string) (ts time.Time, stream string, full bool, msg string, ok bool) {
	p := strings.SplitN(line, " ", 4)
	if len(p) < 3 || (p[2] != "F" && p[2] != "P") {
		return ts, "", false, "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, p[0])
	if err != nil {
		return ts, "", false, "", false
	}
	if len(p) == 4 {
		msg = p[3]
	}
	return ts, p[1], p[2] == "F", msg, true
}

// podLogLabels reads namespace, pod and container out of a path like
// /var/log/pods/<namespace>_<pod>_<uid>/<container>/0.log.
func podLogLabels(path string) (map[string]string, bool) {
	containerDir := filepath.Dir(path)
	parts := strings.Split(filepath.Base(filepath.Dir(containerDir)), "_")
	if len(parts) != 3 {
		return nil, false
	}
	return map[string]string{"source": "kubernetes", "namespace": parts[0], "pod": parts[1],
		"container": filepath.Base(containerDir)}, true
}
