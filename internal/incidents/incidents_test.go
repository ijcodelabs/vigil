package incidents

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	_ "modernc.org/sqlite"
)

func newTracker(t *testing.T) *Tracker {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	tr, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func alert(labels map[string]string, silenced bool) Signal {
	return FromAlert(alerting.ActiveAlert{Alert: alerting.Alert{Labels: labels, Annotations: map[string]string{}}, Silenced: silenced})
}

var (
	t0         = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	ctx        = context.Background()
	hostDown   = map[string]string{"alertname": "HostDown", "agent": "web-1", "severity": "critical"}
	ctrDown    = map[string]string{"alertname": "ContainerDown", "agent": "web-1", "container": "shop-api", "severity": "critical"}
	diskFull   = map[string]string{"alertname": "DiskFull", "agent": "web-1", "severity": "warning"}
	otherHost  = map[string]string{"alertname": "DiskFull", "agent": "db-1", "severity": "warning"}
	detection  = map[string]string{"alertname": "ShadowRead", "agent": "web-1", "source": "security", "severity": "warning"}
	probeLabel = map[string]string{"alertname": "ProbeFailing", "agent": "web-1", "probe": "shop", "severity": "critical"}
)

func TestFromAlert(t *testing.T) {
	for _, c := range []struct {
		labels         map[string]string
		kind, resource string
	}{
		{hostDown, KindHostDown, "host:web-1"},
		{ctrDown, KindAlert, "container:web-1/shop-api"},
		{probeLabel, KindAlert, "check:web-1/shop"},
		{detection, KindSecurity, "host:web-1"},
		{map[string]string{"alertname": "Slow", "service": "shop-api"}, KindAlert, "service:shop-api"},
		{map[string]string{"alertname": "SLOBurnFast", "slo": "shop-api-availability", "service": "shop-api", "severity": "critical"}, KindSLO, "slo:shop-api-availability"},
		// A rule of someone's own with an slo label is not an SLO incident.
		{map[string]string{"alertname": "CheckoutSlow", "slo": "shop", "service": "shop-api"}, KindAlert, "service:shop-api"},
	} {
		s := alert(c.labels, false)
		if s.Kind != c.kind || s.Resource != c.resource || s.Title == "" {
			t.Errorf("%v: kind %q resource %q title %q", c.labels, s.Kind, s.Resource, s.Title)
		}
	}
	// Two alerts on one host that differ in a label are two incidents.
	a, b := alert(diskFull, false), alert(map[string]string{"alertname": "DiskFull", "agent": "web-1", "severity": "warning", "mount": "/data"}, false)
	if a.Key == b.Key {
		t.Error("different label sets share a key")
	}
}

// Open, resolved after the grace period, back within the reopen window as
// the same incident, and a new one after it.
func TestLifecycle(t *testing.T) {
	tr := newTracker(t)
	sync := func(at time.Time, sigs ...Signal) {
		t.Helper()
		if err := tr.Sync(ctx, sigs, at, true); err != nil {
			t.Fatal(err)
		}
	}
	disk := alert(diskFull, false)
	sync(t0, disk)
	open, _ := tr.Open(ctx)
	if len(open) != 1 || open[0].State != StateOpen {
		t.Fatalf("after the first signal: %+v", open)
	}
	id := open[0].ID

	sync(t0.Add(30 * time.Second)) // missing, but within the grace period
	if open, _ = tr.Open(ctx); len(open) != 1 {
		t.Fatalf("resolved within the grace period: %+v", open)
	}
	sync(t0.Add(2 * time.Minute))
	if open, _ = tr.Open(ctx); len(open) != 0 {
		t.Fatalf("not resolved after the grace period: %+v", open)
	}
	if res, _ := tr.Resolved(ctx, t0, 10); len(res) != 1 || res[0].ID != id {
		t.Fatalf("resolved list: %+v", res)
	}

	sync(t0.Add(10*time.Minute), disk)
	open, _ = tr.Open(ctx)
	if len(open) != 1 || open[0].ID != id || open[0].Reopened != 1 {
		t.Fatalf("not reopened as the same incident: %+v", open)
	}
	sync(t0.Add(12 * time.Minute))
	sync(t0.Add(2*time.Hour), disk)
	if open, _ = tr.Open(ctx); len(open) != 1 || open[0].ID == id {
		t.Fatalf("an old incident was reopened instead of a new one: %+v", open)
	}

	_, events, err := tr.Get(ctx, id)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if err != nil || len(types) != 4 || types[0] != "opened" || types[1] != "resolved" || types[2] != "reopened" || types[3] != "resolved" {
		t.Errorf("history: %v %v", types, err)
	}
}

// While a host is down its other incidents hang under it, and stand on
// their own again when it is back.
func TestHostDownHoldsTheHostsIncidents(t *testing.T) {
	tr := newTracker(t)
	tr.Sync(ctx, []Signal{alert(ctrDown, false), alert(otherHost, false)}, t0, true)
	tr.Sync(ctx, []Signal{alert(ctrDown, false), alert(otherHost, false), alert(hostDown, false)}, t0.Add(time.Minute), true)
	open, _ := tr.Open(ctx)
	if len(open) != 2 || open[0].Kind != KindHostDown || len(open[0].Children) != 1 || open[0].Children[0].Resource != "container:web-1/shop-api" {
		t.Fatalf("while down: %+v", open)
	}
	tr.Sync(ctx, []Signal{alert(ctrDown, false), alert(otherHost, false)}, t0.Add(5*time.Minute), true)
	if open, _ = tr.Open(ctx); len(open) != 2 || open[0].Kind == KindHostDown || len(open[0].Children) != 0 {
		t.Fatalf("after the host is back: %+v", open)
	}
}

func TestAcknowledgeAndSilence(t *testing.T) {
	tr := newTracker(t)
	tr.Sync(ctx, []Signal{alert(diskFull, false), alert(probeLabel, false)}, t0, true)
	open, _ := tr.Open(ctx)
	disk := open[1] // the critical probe ranks first
	if open[0].Resource != "check:web-1/shop" {
		t.Fatalf("ranking: %+v", open)
	}
	if err := tr.Acknowledge(ctx, disk.ID, "ada", "cleaning up", t0); err != nil {
		t.Fatal(err)
	}
	tr.Sync(ctx, []Signal{alert(diskFull, false), alert(probeLabel, true)}, t0.Add(time.Minute), true)
	open, _ = tr.Open(ctx)
	// Open before acknowledged, unsilenced before silenced.
	if open[0].ID != disk.ID || open[0].State != StateAcknowledged || open[0].AckBy != "ada" || !open[1].Silenced {
		t.Fatalf("after ack and silence: %+v", open)
	}
	if err := tr.Acknowledge(ctx, 999, "ada", "", t0); err != ErrNotFound {
		t.Errorf("ack of nothing: %v", err)
	}
}

// "Since you were last here" counts what happened after a person's marker,
// and the marker never moves back.
func TestSeen(t *testing.T) {
	tr := newTracker(t)
	tr.Sync(ctx, []Signal{alert(diskFull, false)}, t0, true)
	s, _ := tr.Since(ctx, 0)
	if s.Opened != 1 || s.Latest == 0 {
		t.Fatalf("since 0: %+v", s)
	}
	tr.MarkSeen(ctx, "ada", s.Latest, t0)
	tr.Sync(ctx, []Signal{alert(detection, false)}, t0.Add(time.Minute), true)
	tr.Sync(ctx, []Signal{alert(detection, false)}, t0.Add(3*time.Minute), true) // the disk incident resolves
	seen, _ := tr.Seen(ctx, "ada")
	if s, _ = tr.Since(ctx, seen); s.Opened != 1 || s.Resolved != 1 {
		t.Fatalf("since the marker: %+v", s)
	}
	tr.MarkSeen(ctx, "ada", 1, t0)
	if again, _ := tr.Seen(ctx, "ada"); again != seen {
		t.Errorf("the marker moved back: %d -> %d", seen, again)
	}
	if n, _ := tr.Seen(ctx, "nobody"); n != 0 {
		t.Errorf("someone who never looked: %d", n)
	}
}

func TestPrune(t *testing.T) {
	tr := newTracker(t)
	tr.Sync(ctx, []Signal{alert(diskFull, false)}, t0, true)
	tr.Sync(ctx, nil, t0.Add(2*time.Minute), true)
	if err := tr.Prune(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, _ := tr.Resolved(ctx, time.Time{}, 10); len(res) != 0 {
		t.Errorf("not pruned: %+v", res)
	}
}

// A HAProxy server's incident hangs under its backend's, and both under the
// host when the host is down.
func TestBackendHoldsItsServers(t *testing.T) {
	tr := newTracker(t)
	backend := alert(map[string]string{"alertname": "HaproxyBackendDown", "agent": "edge", "proxy": "shop", "severity": "critical"}, false)
	server := alert(map[string]string{"alertname": "HaproxyServerDown", "agent": "edge", "proxy": "shop", "server": "web-2", "severity": "warning"}, false)
	if backend.Resource != "backend:edge/shop" || server.Resource != "backend:edge/shop/web-2" || server.ParentResource != backend.Resource {
		t.Fatalf("resources: %+v %+v", backend, server)
	}
	tr.Sync(ctx, []Signal{server, backend}, t0, true)
	open, _ := tr.Open(ctx)
	if len(open) != 1 || open[0].Resource != "backend:edge/shop" || len(open[0].Children) != 1 {
		t.Fatalf("server under backend: %+v", open)
	}
	down := alert(map[string]string{"alertname": "HostDown", "agent": "edge", "severity": "critical"}, false)
	tr.Sync(ctx, []Signal{server, backend, down}, t0.Add(time.Minute), true)
	if open, _ = tr.Open(ctx); len(open) != 1 || open[0].Kind != KindHostDown || len(open[0].Children) != 2 {
		t.Fatalf("both under the down host: %+v", open)
	}
	tr.Sync(ctx, []Signal{server}, t0.Add(5*time.Minute), true) // backend back, one server still down
	if open, _ = tr.Open(ctx); len(open) != 1 || open[0].Resource != "backend:edge/shop/web-2" {
		t.Fatalf("the server on its own: %+v", open)
	}
}

// An alert that keeps coming back is listed with how often and how long.
func TestNoisy(t *testing.T) {
	tr := newTracker(t)
	flap := alert(probeLabel, false)
	at := t0
	for i := 0; i < 6; i++ { // fires 3 minutes, quiet an hour (past the reopen window)
		tr.Sync(ctx, []Signal{flap}, at, true)
		tr.Sync(ctx, []Signal{flap}, at.Add(2*time.Minute), true)
		tr.Sync(ctx, nil, at.Add(3*time.Minute+30*time.Second), true)
		at = at.Add(time.Hour)
	}
	tr.Sync(ctx, []Signal{alert(diskFull, false)}, at, true) // once: not noisy
	list, err := tr.Noisy(ctx, t0, 5)
	if err != nil || len(list) != 1 || list[0].Alertname != "ProbeFailing" || list[0].Firings != 6 || list[0].MedianMin < 3 || list[0].MedianMin > 4 {
		t.Fatalf("noisy: %+v %v", list, err)
	}
}

// Right after a restart the alerts are not all back; nothing is resolved
// until the server says so.
func TestNoResolveWhileWarmingUp(t *testing.T) {
	tr := newTracker(t)
	tr.Sync(ctx, []Signal{alert(diskFull, false)}, t0, true)
	tr.Sync(ctx, nil, t0.Add(5*time.Minute), false)
	if open, _ := tr.Open(ctx); len(open) != 1 {
		t.Fatalf("resolved while warming up: %+v", open)
	}
	tr.Sync(ctx, []Signal{alert(diskFull, false)}, t0.Add(6*time.Minute), true)
	if open, _ := tr.Open(ctx); len(open) != 1 || open[0].Reopened != 0 {
		t.Fatalf("after warming up: %+v", open)
	}
}
