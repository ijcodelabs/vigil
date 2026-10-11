package server_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/server"
)

func TestDetectionsAndCaptures(t *testing.T) {
	store := &fakeStore{}
	backend := httptest.NewServer(store)
	defer backend.Close()

	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret",
		ExternalURL: "http://vigil.example", HeartbeatTimeout: config.Duration(time.Minute), CapturesMaxBytes: 1 << 30}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = backend.URL, backend.URL
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	call := func(method, path, token string, body []byte, headers ...string) (int, string) {
		req, _ := http.NewRequest(method, central.URL+path, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Vigil-Agent", "web-01")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	trigger := &events.Event{Time: now, Type: events.TypeExec, PID: 777, PPID: 12, UID: 33, EUID: 33, Comm: "sh",
		Exe: "/bin/sh", Args: []string{"sh", "-c", "id"}, ParentComm: "nginx", Container: "web", ContainerID: "abcdef012345"}
	const capID = "0123456789abcdef"
	batch := events.EncodeDetections([]events.Detection{
		{Time: now, Rule: "shell_in_container", Severity: "critical", Summary: "/bin/sh started in web by nginx", CaptureID: capID, Event: trigger},
		{Time: now, Rule: "shadow_read", Severity: "warning", Summary: "open by cat",
			Event: &events.Event{Time: now, Type: events.TypeOpen, PID: 778, Comm: "cat", Path: "/etc/shadow"}},
	})
	if code, body := call("POST", "/api/v1/events", "ingest", batch); code != http.StatusNoContent {
		t.Fatalf("post detections: %d %s", code, body)
	}

	// Stored as log entries...
	store.mu.Lock()
	var shell map[string]string
	for _, row := range store.logs {
		if row["rule"] == "shell_in_container" {
			shell = row
		}
	}
	nlogs, sf := len(store.logs), store.streamFields
	store.mu.Unlock()
	if nlogs != 2 || shell["_msg"] != "/bin/sh started in web by nginx" || shell["source"] != "security" || shell["severity"] != "critical" ||
		shell["agent"] != "web-01" || shell["exe"] != "/bin/sh" || shell["cmdline"] != "sh -c id" || shell["container"] != "web" ||
		shell["capture_id"] != capID || shell["pid"] != "777" || shell["parent_comm"] != "nginx" {
		t.Errorf("%d log rows; shell row: %v", nlogs, shell)
	}
	if sf != "agent,rule,severity,source" {
		t.Errorf("stream fields: %q", sf)
	}
	// ...and raised as alerts at once.
	code, body := call("GET", "/alertmanager/api/v2/alerts", "admin-token-secret", nil)
	var alerts []struct {
		Labels, Annotations map[string]string
	}
	json.Unmarshal([]byte(body), &alerts)
	found := false
	for _, a := range alerts {
		if a.Labels["alertname"] == "shell_in_container" {
			found = a.Labels["agent"] == "web-01" && a.Labels["container"] == "web" && a.Labels["severity"] == "critical" &&
				a.Labels["source"] == "security" && a.Annotations["capture"] == "http://vigil.example/#/captures/"+capID
		}
	}
	if code != 200 || len(alerts) != 2 || !found {
		t.Errorf("alerts: %d %s", code, body)
	}

	// Captures: upload, list, read back.
	capture := &events.Capture{Trigger: trigger, Events: []*events.Event{trigger},
		Meta: events.CaptureMeta{ID: capID, Rule: "shell_in_container", Severity: "critical", Summary: "/bin/sh started in web by nginx",
			Time: now, From: now.Add(-30 * time.Second), Until: now.Add(30 * time.Second), Events: 1}}
	header := func(m events.CaptureMeta) string {
		b, _ := json.Marshal(m)
		return base64.StdEncoding.EncodeToString(b)
	}
	if code, body := call("POST", "/api/v1/captures", "ingest", capture.Encode(), "X-Vigil-Capture", header(capture.Meta)); code != http.StatusNoContent {
		t.Fatalf("upload capture: %d %s", code, body)
	}
	// The agent retrying the same upload is fine and does not duplicate it.
	if code, _ := call("POST", "/api/v1/captures", "ingest", capture.Encode(), "X-Vigil-Capture", header(capture.Meta)); code != http.StatusNoContent {
		t.Errorf("repeated upload: %d", code)
	}
	bad := capture.Meta
	bad.ID = "../../etc/passwd"
	if code, _ := call("POST", "/api/v1/captures", "ingest", capture.Encode(), "X-Vigil-Capture", header(bad)); code != http.StatusBadRequest {
		t.Errorf("capture id with path characters: %d", code)
	}
	if code, _ := call("POST", "/api/v1/captures", "ingest", capture.Encode()); code != http.StatusBadRequest {
		t.Errorf("capture without metadata: %d", code)
	}

	code, body = call("GET", "/api/v1/captures", "admin-token-secret", nil)
	var list []server.CaptureInfo
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list) != 1 || list[0].ID != capID || list[0].Agent != "web-01" || list[0].Events != 1 || !list[0].Time.Equal(now) || list[0].Size == 0 {
		t.Fatalf("list: %d %s", code, body)
	}
	// Go's client unpacks the gzip the same way a browser does.
	code, body = call("GET", "/api/v1/captures/"+capID, "admin-token-secret", nil)
	var back events.Capture
	if err := json.Unmarshal([]byte(body), &back); code != 200 || err != nil || back.Meta.ID != capID || back.Trigger.Exe != "/bin/sh" || len(back.Events) != 1 {
		t.Errorf("read capture: %d %v", code, err)
	}
	for _, id := range []string{"ffffffffffffffff", "..%2f..%2fvigil.db", "short"} {
		if code, _ := call("GET", "/api/v1/captures/"+id, "admin-token-secret", nil); code != http.StatusNotFound {
			t.Errorf("GET capture %q: %d", id, code)
		}
	}
	if code, _ := call("GET", "/api/v1/captures", "ingest", nil); code != http.StatusUnauthorized {
		t.Errorf("ingest token lists captures: %d", code)
	}

	// Retention: with a tiny budget only the newest capture survives.
	cfg.CapturesMaxBytes = 1
	newer := *capture
	newer.Meta.ID, newer.Meta.Time = "00000000000000aa", now.Add(time.Minute)
	if code, _ := call("POST", "/api/v1/captures", "ingest", newer.Encode(), "X-Vigil-Capture", header(newer.Meta)); code != http.StatusNoContent {
		t.Fatalf("second upload: %d", code)
	}
	_, body = call("GET", "/api/v1/captures", "admin-token-secret", nil)
	json.Unmarshal([]byte(body), &list)
	if len(list) != 1 || list[0].ID != "00000000000000aa" {
		t.Errorf("after retention: %s", body)
	}
	if code, _ := call("GET", "/api/v1/captures/"+capID, "admin-token-secret", nil); code != http.StatusNotFound {
		t.Errorf("trimmed capture still served: %d", code)
	}
	if code, _ := call("DELETE", "/api/v1/captures/00000000000000aa", "admin-token-secret", nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	_, body = call("GET", "/api/v1/captures", "admin-token-secret", nil)
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("after delete: %s", body)
	}
}
