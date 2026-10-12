package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/prometheus/alertmanager/silence/silencepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// hook collects what a webhook receiver is sent.
type hook struct {
	mu   sync.Mutex
	msgs []string // "status alertname summary"
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var m struct {
		Alerts []struct {
			Status      string            `json:"status"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"alerts"`
	}
	json.NewDecoder(r.Body).Decode(&m)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range m.Alerts {
		h.msgs = append(h.msgs, a.Status+" "+a.Labels["alertname"]+" "+a.Annotations["summary"])
	}
}

func (h *hook) find(prefix string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.HasPrefix(m, prefix) {
			return m
		}
	}
	return ""
}

func (h *hook) wait(t *testing.T, prefix string) string {
	t.Helper()
	for range 100 {
		if m := h.find(prefix); m != "" {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no message starting with %q; got %q", prefix, h.msgs)
	return ""
}

func TestSilenceNotifications(t *testing.T) {
	ops, db := &hook{}, &hook{}
	opsSrv, dbSrv := httptest.NewServer(ops), httptest.NewServer(db)
	defer opsSrv.Close()
	defer dbSrv.Close()

	am, err := New(t.TempDir(), "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(fmt.Sprintf(`
route:
  receiver: ops
  group_by: [alertname, agent]
  group_wait: 0s
  group_interval: 1s
  routes:
    - matchers: [team="db"]
      receiver: db
receivers:
  - name: ops
    webhook_configs: [{url: %q, send_resolved: true}]
  - name: db
    webhook_configs: [{url: %q, send_resolved: true}]
`, opsSrv.URL, dbSrv.URL)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alert := func(name, agent string, ended bool, extra ...string) Alert {
		a := Alert{Labels: map[string]string{"alertname": name, "agent": agent}, Annotations: map[string]string{"summary": name + " on " + agent},
			StartsAt: time.Now().Add(-time.Minute)}
		for i := 0; i+1 < len(extra); i += 2 {
			a.Labels[extra[i]] = extra[i+1]
		}
		if ended {
			a.EndsAt = time.Now().Add(-time.Second)
		}
		return a
	}
	silence := func(by, comment string, matchers ...*pb.Matcher) string {
		t.Helper()
		s := &pb.Silence{Matchers: matchers, StartsAt: timestamppb.Now(), EndsAt: timestamppb.New(time.Now().Add(2 * time.Hour)), CreatedBy: by, Comment: comment}
		if err := am.silences.Set(ctx, s); err != nil {
			t.Fatal(err)
		}
		am.NotifySilence(ctx, s.Id)
		return s.Id
	}
	eq := func(name, value string) *pb.Matcher {
		return &pb.Matcher{Type: pb.Matcher_EQUAL, Name: name, Pattern: value}
	}

	// An alert fires and its contact is told.
	if err := am.Send(ctx, []Alert{alert("DiskFull", "web-1", false)}); err != nil {
		t.Fatal(err)
	}
	ops.wait(t, "firing DiskFull")

	// Silencing it tells the same contact who did it, for how long and why.
	silence("nora", "disk is being replaced", eq("alertname", "DiskFull"), eq("agent", "web-1"))
	m := ops.wait(t, "firing "+SilencedAlertName)
	for _, want := range []string{"nora silenced", `alertname="DiskFull"`, `agent="web-1"`, "disk is being replaced", "1 firing alert is covered"} {
		if !strings.Contains(m, want) {
			t.Errorf("message about the silence lacks %q: %s", want, m)
		}
	}
	if db.find("firing "+SilencedAlertName) != "" {
		t.Errorf("a contact the alert is not routed to was told about its silence: %q", db.msgs)
	}

	// The alert ends while silenced: the contact that knew it fired hears that it ended.
	if err := am.Send(ctx, []Alert{alert("DiskFull", "web-1", true)}); err != nil {
		t.Fatal(err)
	}
	if m := ops.wait(t, "resolved DiskFull"); !strings.Contains(m, "ended while silenced by nora") {
		t.Errorf("resolution lacks the note about the silence: %s", m)
	}
	// Rule evaluation repeats a resolution for a while; it is reported once.
	am.Send(ctx, []Alert{alert("DiskFull", "web-1", true)})

	// Planned work: a silence that covers nothing yet goes to the contact
	// its labels are routed to, and an alert that begins and ends inside it
	// is never mentioned.
	silence("nora", "database upgrade", eq("team", "db"))
	if m := db.wait(t, "firing "+SilencedAlertName); !strings.Contains(m, "No alert is covered right now") {
		t.Errorf("message about a silence for planned work: %s", m)
	}
	am.Send(ctx, []Alert{alert("ReplicaLag", "db-1", false, "team", "db")})
	am.Send(ctx, []Alert{alert("ReplicaLag", "db-1", true, "team", "db")})
	time.Sleep(1500 * time.Millisecond)
	if db.find("firing ReplicaLag") != "" || db.find("resolved ReplicaLag") != "" {
		t.Errorf("an alert that lived inside a silence was reported: %q", db.msgs)
	}
	ops.mu.Lock()
	n := 0
	for _, m := range ops.msgs {
		if strings.HasPrefix(m, "resolved DiskFull") {
			n++
		}
	}
	ops.mu.Unlock()
	if n != 1 {
		t.Errorf("the end of DiskFull was reported %d times, want once: %q", n, ops.msgs)
	}

	// Switched off, a new silence is quiet.
	am.NotifyAboutSilences(false)
	before := len(ops.msgs)
	silence("nora", "quiet", eq("alertname", "Anything"))
	time.Sleep(300 * time.Millisecond)
	if len(ops.msgs) != before {
		t.Errorf("a silence was announced although that is switched off: %q", ops.msgs[before:])
	}
}
