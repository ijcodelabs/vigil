// Package logwire is the agent→server log batch format: gzip-compressed
// newline-delimited JSON, one model.LogEntry per line.
package logwire

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"

	"github.com/ijcodelabs/vigil/internal/model"
)

func Encode(entries []model.LogEntry) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for i := range entries {
		enc.Encode(&entries[i])
	}
	zw.Close()
	return buf.Bytes()
}

var ErrTooLarge = errors.New("logwire: batch too large when decompressed")

// Decode reads a batch, refusing to inflate more than max bytes.
func Decode(r io.Reader, max int64) ([]model.LogEntry, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	lr := &io.LimitedReader{R: zr, N: max + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var out []model.LogEntry
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e model.LogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if lr.N <= 0 {
		return nil, ErrTooLarge
	}
	return out, nil
}
