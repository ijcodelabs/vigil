package logs

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
)

// Record is what a source hands to the manager: an entry plus the position
// to remember once that entry is safely spooled. Entry is nil for lines that
// were filtered out but still advance the position.
type Record struct {
	Entry  *model.LogEntry
	Key    string // position key; empty means nothing to remember
	Pos    string
	Delete bool // forget Key (file or container is gone)
}

// block is one raw line (or several joined ones) on its way to a template.
type block struct {
	text   string
	time   time.Time         // from the source format, zero if unknown
	fields map[string]string // from the source format
	labels map[string]string // extra stream labels for this block
	pos    string            // position after this block
}

const maxMultilineLines = 500

// assembler joins continuation lines onto the line that started an entry.
// It keeps no clock: the caller flushes when the source goes quiet.
type assembler struct {
	start *regexp.Regexp
	cur   block
	lines []string
}

// add returns the entries completed by this line.
func (a *assembler) add(b block) []block {
	if a.start == nil {
		return []block{b}
	}
	var out []block
	if len(a.lines) > 0 && (a.start.MatchString(b.text) || len(a.lines) >= maxMultilineLines) {
		out = append(out, a.take())
	}
	if len(a.lines) == 0 {
		a.cur = b
	}
	a.lines = append(a.lines, b.text)
	a.cur.pos = b.pos
	return out
}

func (a *assembler) take() block {
	b := a.cur
	b.text = strings.Join(a.lines, "\n")
	a.lines, a.cur = nil, block{}
	return b
}

// flush returns the entry still being assembled, if any.
func (a *assembler) flush() (block, bool) {
	if len(a.lines) == 0 {
		return block{}, false
	}
	return a.take(), true
}

// sink turns blocks of one source into records.
type sink struct {
	ctx    context.Context
	out    chan<- Record
	key    string
	tmpl   *Template
	stream map[string]string
}

func (s *sink) send(r Record) bool {
	select {
	case s.out <- r:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// emit runs the template and sends the result; false means shutting down.
func (s *sink) emit(b block) bool {
	rec := Record{Key: s.key, Pos: b.pos}
	if strings.TrimSpace(b.text) == "" {
		return s.send(rec)
	}
	msg, ts, labels, fields, ok := s.tmpl.Process(b.text)
	if !ok {
		return s.send(rec)
	}
	e := &model.LogEntry{Msg: msg, Stream: map[string]string{}, Fields: fields}
	for _, m := range []map[string]string{s.stream, b.labels, labels} {
		for k, v := range m {
			e.Stream[k] = v
		}
	}
	for k, v := range b.fields {
		if _, clash := e.Fields[k]; !clash {
			e.Fields[k] = v
		}
	}
	switch {
	case !ts.IsZero():
		e.Time = ts.UnixNano()
	case !b.time.IsZero():
		e.Time = b.time.UnixNano()
	default:
		e.Time = time.Now().UnixNano()
	}
	rec.Entry = e
	return s.send(rec)
}

// pump drives a streaming source: it joins multi-line entries, flushes the
// pending one when the stream goes quiet for a second, and returns the
// position of the last block read once lines is closed or ctx ends.
func pump(s *sink, lines <-chan block) (last string) {
	asm := assembler{start: s.tmpl.start}
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return last
		case b, ok := <-lines:
			if !ok {
				if b, pending := asm.flush(); pending {
					s.emit(b)
				}
				return last
			}
			if b.pos != "" {
				last = b.pos
			}
			for _, done := range asm.add(b) {
				if !s.emit(done) {
					return last
				}
			}
		case <-idle.C:
			if b, ok := asm.flush(); ok && !s.emit(b) {
				return last
			}
		}
	}
}
