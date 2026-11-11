package alerting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendTestAndRoute(t *testing.T) {
	ops, db, spare := &hook{}, &hook{}, &hook{}
	opsSrv, dbSrv, spareSrv := httptest.NewServer(ops), httptest.NewServer(db), httptest.NewServer(spare)
	defer opsSrv.Close()
	defer dbSrv.Close()
	defer spareSrv.Close()
	// A contact whose address carries a token, and that refuses everything.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer refused.Close()

	am, err := New(t.TempDir(), "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	conf := func(dbURL string) string {
		return fmt.Sprintf(`
route:
  receiver: ops
  group_by: [alertname, agent]
  routes:
    - matchers: [team="db"]
      receiver: db
      continue: true
    - matchers: [team="db", severity="critical"]
      receiver: ops
receivers:
  - name: ops
    webhook_configs: [{url: %q}, {url: %q}]
  - name: db
    webhook_configs: [{url: %q}]
  - name: unused
    webhook_configs: [{url: %q}]
`, opsSrv.URL, refused.URL+"/hooks/secret-token-123", dbURL, spareSrv.URL)
	}
	if err := am.Apply(conf(dbSrv.URL)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Every integration in use is listed before it sent anything; the
	// unused contact is not.
	rows := map[string]Delivery{}
	for _, d := range am.Deliveries() {
		rows[fmt.Sprintf("%s/%s/%d", d.Receiver, d.Integration, d.Index)] = d
	}
	if len(rows) != 3 || rows["ops/webhook/1"].Receiver == "" || rows["db/webhook/0"].Receiver == "" {
		t.Fatalf("deliveries before any send: %v", rows)
	}

	// A test reaches exactly that integration, and counts.
	if err := am.SendTest(ctx, "", Target{"db", "webhook", 0}, "alice"); err != nil {
		t.Fatal(err)
	}
	if m := db.wait(t, "firing "+TestAlertName); !strings.Contains(m, "sent by alice") {
		t.Errorf("test message: %q", m)
	}
	if ops.find("") != "" {
		t.Errorf("another contact was told: %q", ops.msgs)
	}
	for _, d := range am.Deliveries() {
		if d.Receiver == "db" && d.Sent != 1 {
			t.Errorf("db delivery not counted: %+v", d)
		}
	}

	// Pressed again at once: refused without sending.
	if err := am.SendTest(ctx, "", Target{"db", "webhook", 0}, "alice"); !errors.Is(err, ErrTooSoon) {
		t.Errorf("second test at once: %v", err)
	}

	// A failure comes back without the address's path, where tokens live.
	err = am.SendTest(ctx, "", Target{"ops", "webhook", 1}, "")
	if err == nil || strings.Contains(err.Error(), "secret-token-123") || !strings.Contains(err.Error(), "403") {
		t.Errorf("refused contact: %v", err)
	}

	// Unknown targets, and a contact no route uses in the saved configuration.
	for _, bad := range []Target{{"nobody", "webhook", 0}, {"ops", "webhook", 5}, {"ops", "email", 0}, {"unused", "webhook", 0}} {
		if err := am.SendTest(ctx, "", bad, ""); !errors.Is(err, ErrNoIntegration) {
			t.Errorf("%v: %v", bad, err)
		}
	}

	// A configuration that is not saved: tested as written, nothing recorded,
	// nothing changed in what is in use.
	if err := am.SendTest(ctx, conf(spareSrv.URL), Target{"unused", "webhook", 0}, "bob"); err != nil {
		t.Fatal(err)
	}
	spare.wait(t, "firing "+TestAlertName)
	for _, d := range am.Deliveries() {
		if d.Receiver == "unused" {
			t.Errorf("a test of an unsaved configuration was recorded: %+v", d)
		}
	}
	if err := am.SendTest(ctx, "route: [", Target{"ops", "webhook", 0}, ""); err == nil {
		t.Error("broken configuration accepted")
	}

	// Who would be told.
	got := func(yaml string, labels map[string]string) string {
		t.Helper()
		r, err := am.Route(ctx, yaml, labels)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range r.Matches {
			names = append(names, fmt.Sprintf("%s:%d", m.Receiver, len(m.Integrations)))
		}
		return strings.Join(names, ",")
	}
	if g := got("", map[string]string{"alertname": "X"}); g != "ops:2" {
		t.Errorf("default route: %s", g)
	}
	if g := got("", map[string]string{"alertname": "X", "team": "db", "severity": "critical"}); g != "db:1,ops:2" {
		t.Errorf("continue: %s", g)
	}
	r, _ := am.Route(ctx, "", map[string]string{"team": "db"})
	if m := r.Matches[0]; m.Route == "" || strings.Join(m.GroupBy, ",") != "agent,alertname" {
		t.Errorf("match details: %+v", m)
	}
	if _, err := am.Route(ctx, "", map[string]string{"": "x"}); err == nil {
		t.Error("invalid label name accepted")
	}
	// The unsaved configuration's routing, not the one in use.
	if g := got("route: {receiver: unused}\nreceivers: [{name: unused}]\n", map[string]string{"team": "db"}); g != "unused:0" {
		t.Errorf("unsaved routing: %s", g)
	}
}
