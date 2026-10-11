package server

import (
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/model"
)

const (
	maxCaptureBody = 64 << 20
	// A detection keeps its alert firing this long; repeats extend it.
	detectionAlertTTL = 10 * time.Minute
)

var captureIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Server) securityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/events", s.ingest(s.bounded(s.handleDetections)))
	mux.HandleFunc("POST /api/v1/captures", s.ingest(s.bounded(s.handleCaptureUpload)))
	mux.HandleFunc("GET /api/v1/captures", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.store.Captures()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("GET /api/v1/captures/{id}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !captureIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(s.capturePath(id))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		// Stored compressed; the browser unpacks it.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		io.Copy(w, f)
	}))
	mux.HandleFunc("DELETE /api/v1/captures/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !captureIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		os.Remove(s.capturePath(id))
		if err := s.store.DeleteCapture(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *Server) capturePath(id string) string {
	return filepath.Join(s.cfg.DataDir, "captures", id+".json.gz")
}

// handleDetections takes what agents' security rules matched. Each
// detection is stored as a log entry (source=security), so it can be
// searched and counted like any log, and raised as an alert right away,
// without waiting for a rule evaluation.
func (s *Server) handleDetections(w http.ResponseWriter, r *http.Request) {
	id, ok := agentID(w, r)
	if !ok {
		return
	}
	zr, err := gzip.NewReader(http.MaxBytesReader(w, r.Body, maxWriteBody))
	if err != nil {
		http.Error(w, "bad detection batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	var (
		entries []model.LogEntry
		alerts  []alerting.Alert
	)
	sc := bufio.NewScanner(io.LimitReader(zr, maxLogBatch))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var d events.Detection
		if json.Unmarshal(sc.Bytes(), &d) != nil || d.Event == nil || d.Rule == "" {
			continue
		}
		ev := d.Event
		fields := map[string]string{"type": ev.Type, "pid": strconv.Itoa(int(ev.PID)), "ppid": strconv.Itoa(int(ev.PPID)),
			"uid": strconv.Itoa(int(ev.UID)), "euid": strconv.Itoa(int(ev.EUID)), "comm": ev.Comm}
		for k, v := range map[string]string{"exe": ev.Exe, "cmdline": strings.Join(ev.Args, " "), "parent_comm": ev.ParentComm,
			"parent_exe": ev.ParentExe, "container": ev.Container, "container_id": ev.ContainerID, "path": ev.Path,
			"addr": ev.Addr, "capture_id": d.CaptureID} {
			if v != "" {
				fields[k] = v
			}
		}
		if ev.Port != 0 {
			fields["port"] = strconv.Itoa(int(ev.Port))
		}
		if ev.Write {
			fields["write"] = "true"
		}
		if ev.NewEUID != nil {
			fields["old_euid"], fields["new_euid"] = strconv.Itoa(int(*ev.OldEUID)), strconv.Itoa(int(*ev.NewEUID))
		}
		entries = append(entries, model.LogEntry{Time: d.Time.UnixNano(), Msg: d.Summary,
			Stream: map[string]string{"source": "security", "rule": d.Rule, "severity": d.Severity}, Fields: fields})

		// The alert is per rule, agent and container, not per process, so a
		// burst from one place is one alert.
		labels := map[string]string{"alertname": d.Rule, "severity": d.Severity, "agent": id, "source": "security"}
		if ev.Container != "" {
			labels["container"] = ev.Container
		}
		ann := map[string]string{"summary": d.Summary}
		if d.CaptureID != "" {
			ann["capture_id"] = d.CaptureID
			ann["capture"] = strings.TrimRight(s.cfg.ExternalURL, "/") + "/#/captures/" + d.CaptureID
		}
		alerts = append(alerts, alerting.Alert{Labels: labels, Annotations: ann, StartsAt: d.Time,
			EndsAt: d.Time.Add(detectionAlertTTL), GeneratorURL: s.cfg.ExternalURL})
	}
	if err := sc.Err(); err != nil {
		http.Error(w, "bad detection batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.forwardLogs(r.Context(), id, entries); err != nil {
		s.log.Warn("forward to log store failed", "agent", id, "err", err)
		http.Error(w, "log store unavailable", http.StatusServiceUnavailable) // agent retries
		return
	}
	if len(alerts) > 0 {
		if err := s.am.Send(r.Context(), alerts); err != nil {
			s.log.Error("raising alerts for detections failed", "agent", id, "err", err)
		}
	}
	s.touch(id, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleCaptureUpload stores a capture file and indexes it. The body is
// the gzip-compressed capture; its description comes in a header.
func (s *Server) handleCaptureUpload(w http.ResponseWriter, r *http.Request) {
	agent, ok := agentID(w, r)
	if !ok {
		return
	}
	var meta events.CaptureMeta
	raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Vigil-Capture"))
	if err != nil || json.Unmarshal(raw, &meta) != nil || !captureIDRe.MatchString(meta.ID) {
		http.Error(w, "missing or invalid X-Vigil-Capture header", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "captures")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	size, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxCaptureBody))
	tmp.Close()
	if err != nil {
		http.Error(w, "capture too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	added, err := s.store.AddCapture(CaptureInfo{ID: meta.ID, Agent: agent, Rule: meta.Rule, Severity: meta.Severity,
		Summary: meta.Summary, Time: meta.Time, From: meta.From, Until: meta.Until, Events: meta.Events, Size: size})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if added { // a repeated upload of the same capture is simply accepted
		if err := os.Rename(tmp.Name(), s.capturePath(meta.ID)); err != nil {
			s.store.DeleteCapture(meta.ID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.trimCaptures()
	}
	s.touch(agent, r)
	w.WriteHeader(http.StatusNoContent)
}

// trimCaptures removes the oldest captures until the total size fits
// captures_max_bytes.
func (s *Server) trimCaptures() {
	list, err := s.store.Captures()
	if err != nil {
		return
	}
	var total int64
	for _, c := range list {
		total += c.Size
	}
	for i := len(list) - 1; i > 0 && total > s.cfg.CapturesMaxBytes; i-- {
		os.Remove(s.capturePath(list[i].ID))
		s.store.DeleteCapture(list[i].ID)
		total -= list[i].Size
		s.log.Info("removed old capture to stay within captures_max_bytes", "id", list[i].ID, "rule", list[i].Rule)
	}
}
