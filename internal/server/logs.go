package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/logwire"
	"github.com/ijcodelabs/vigil/internal/model"
)

const maxLogBatch = 64 << 20 // decompressed

// reserved are the log store's own field names; agents may not set them.
var reserved = map[string]bool{"_msg": true, "_time": true, "_stream": true, "_stream_id": true, "agent": true}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := agentID(w, r)
	if !ok {
		return
	}
	entries, err := logwire.Decode(http.MaxBytesReader(w, r.Body, maxWriteBody), maxLogBatch)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, logwire.ErrTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "bad log batch: "+err.Error(), code)
		return
	}
	if rate := s.cfg.Limits.For(id).LogBytesPerSecond; rate > 0 {
		size := 0
		for _, e := range entries {
			size += len(e.Msg)
			for k, v := range e.Fields {
				size += len(k) + len(v)
			}
		}
		if !s.limiter.allow(id, signalLogs, size, rate, time.Now()) {
			overLimit(w, "bytes of logs", rate)
			return
		}
	}
	if err := s.forwardLogs(r.Context(), id, entries); err != nil {
		s.log.Warn("forward to log store failed", "agent", id, "err", err)
		http.Error(w, "log store unavailable", http.StatusServiceUnavailable) // agent retries
		return
	}
	s.touch(id, r)
	w.WriteHeader(http.StatusNoContent)
}

// forwardLogs writes entries to the log store as JSON lines. Every entry is
// stamped with agent=<id> here, so an agent cannot write under another name.
func (s *Server) forwardLogs(ctx context.Context, agentID string, entries []model.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	streamFields := map[string]bool{"agent": true}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		row := make(map[string]string, len(e.Fields)+len(e.Stream)+3)
		for k, v := range e.Fields {
			if !reserved[k] {
				row[k] = v
			}
		}
		for k, v := range e.Stream { // a stream label wins over a field of the same name
			if !reserved[k] {
				row[k] = v
				streamFields[k] = true
			}
		}
		row["_msg"] = e.Msg
		row["_time"] = time.Unix(0, e.Time).UTC().Format(time.RFC3339Nano)
		row["agent"] = agentID
		enc.Encode(row)
	}
	names := make([]string, 0, len(streamFields))
	for k := range streamFields {
		names = append(names, k)
	}
	sort.Strings(names)

	u := s.logs.JoinPath("/insert/jsonline")
	u.RawQuery = url.Values{"_stream_fields": {strings.Join(names, ",")},
		"_msg_field": {"_msg"}, "_time_field": {"_time"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
