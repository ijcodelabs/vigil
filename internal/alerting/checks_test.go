package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTelegram answers getChat for chat 42 only and records what is sent.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []string // request bodies of sendMessage
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getChat"):
		if r.URL.Query().Get("chat_id") == "42" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":42,"type":"group"}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.sent = append(f.sent, string(b))
		f.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":42},"date":0,"text":"x"}}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTelegram) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func checkByKey(t *testing.T, am *Alertmanager, receiver, integration string) Delivery {
	t.Helper()
	for _, d := range am.Deliveries() {
		if d.Receiver == receiver && d.Integration == integration {
			return d
		}
	}
	t.Fatalf("no delivery for %s/%s in %+v", receiver, integration, am.Deliveries())
	return Delivery{}
}

// The silent check posts nothing and tells a working Telegram chat, a chat
// the bot cannot see, a reachable and an unreachable webhook, and a kind
// it cannot check apart.
func TestSilentChecks(t *testing.T) {
	tg := &fakeTelegram{}
	tgs := httptest.NewServer(tg)
	defer tgs.Close()
	hk := &hook{}
	hooks := httptest.NewServer(hk)
	defer hooks.Close()
	// An address nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String() + "/hook"
	l.Close()

	am, err := New(t.TempDir(), "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(fmt.Sprintf(`
route:
  receiver: good
  routes:
    - {receiver: lost, matchers: [x="1"]}
    - {receiver: down, matchers: [x="2"]}
    - {receiver: other, matchers: [x="3"]}
receivers:
  - name: good
    telegram_configs: [{api_url: %q, bot_token: not-a-real-token, chat_id: 42}]
    webhook_configs: [{url: %q}]
  - name: lost
    telegram_configs: [{api_url: %q, bot_token: not-a-real-token, chat_id: 7}]
  - name: down
    webhook_configs: [{url: %q}]
  - name: other
    pagerduty_configs: [{routing_key: not-a-real-key}]
`, tgs.URL, hooks.URL, tgs.URL, dead)); err != nil {
		t.Fatal(err)
	}
	am.RunChecks(context.Background(), CheckOptions{Interval: time.Minute})

	for _, c := range []struct{ receiver, integration, state, err string }{
		{"good", "telegram", CheckOK, ""},
		{"good", "webhook", CheckOK, ""},
		{"lost", "telegram", CheckFailing, "chat not found"},
		{"down", "webhook", CheckFailing, "refused"},
		{"other", "pagerduty", CheckNone, ""},
	} {
		d := checkByKey(t, am, c.receiver, c.integration)
		if d.Check == nil || d.Check.State != c.state || !strings.Contains(d.Check.Error, c.err) || d.Check.LastCheck.IsZero() {
			t.Errorf("%s/%s: %+v, want %s %q", c.receiver, c.integration, d.Check, c.state, c.err)
		}
		if d.Check != nil && strings.Contains(d.Check.Error, "not-a-real-token") {
			t.Errorf("%s/%s: the token shows in %q", c.receiver, c.integration, d.Check.Error)
		}
	}
	if m := tg.messages(); len(m) != 0 {
		t.Errorf("the silent check sent %v", m)
	}
	if len(hk.msgs) != 0 {
		t.Errorf("the silent check posted to the webhook: %v", hk.msgs)
	}
}

// A real message goes out once nothing has got through for SendInterval:
// not at once for a new contact, silent on Telegram, counted as a send, and
// remembered across a restart.
func TestScheduledSend(t *testing.T) {
	tg := &fakeTelegram{}
	tgs := httptest.NewServer(tg)
	defer tgs.Close()
	hk := &hook{}
	hooks := httptest.NewServer(hk)
	defer hooks.Close()
	conf := fmt.Sprintf(`
route: {receiver: team, group_by: [alertname], group_wait: 0s, group_interval: 1s}
receivers:
  - name: team
    telegram_configs: [{api_url: %q, bot_token: not-a-real-token, chat_id: 42}]
    webhook_configs: [{url: %q}]
`, tgs.URL, hooks.URL)
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := CheckOptions{SendInterval: time.Hour}

	am, err := New(dir, "http://vigil.example.com", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := am.Apply(conf); err != nil {
		t.Fatal(err)
	}
	am.RunChecks(context.Background(), opts)
	if m := tg.messages(); len(m) != 0 {
		t.Fatalf("a new contact got a scheduled message at once: %v", m)
	}
	if d := checkByKey(t, am, "team", "telegram"); d.Check.NextSend.Before(time.Now().Add(50 * time.Minute)) {
		t.Errorf("next send %v, want about an hour from now", d.Check.NextSend)
	}
	am.Close()

	// Two hours later, as far as the saved file knows.
	var saved struct {
		Proof map[string]time.Time `json:"proof"`
		Since map[string]time.Time `json:"since"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "delivery-checks.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &saved)
	if len(saved.Since) != 2 {
		t.Fatalf("saved: %s", b)
	}
	for k := range saved.Since {
		saved.Since[k] = time.Now().Add(-2 * time.Hour)
	}
	b, _ = json.Marshal(saved)
	os.WriteFile(filepath.Join(dir, "delivery-checks.json"), b, 0o600)

	am, err = New(dir, "http://vigil.example.com", log)
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(conf); err != nil {
		t.Fatal(err)
	}
	am.RunChecks(context.Background(), opts)
	m := tg.messages()
	if len(m) != 1 || !strings.Contains(m[0], CheckAlertName) || !strings.Contains(m[0], `"disable_notification":"true"`) {
		t.Fatalf("telegram got %v, want one silent check message", m)
	}
	if hk.find("firing "+CheckAlertName) == "" {
		t.Errorf("the webhook got no check message: %v", hk.msgs)
	}
	d := checkByKey(t, am, "team", "telegram")
	if d.Sent != 1 || d.Check.LastProof.IsZero() || d.Check.SendError != "" {
		t.Errorf("after the send: %+v %+v", d, d.Check)
	}
	// Proved now: the next run sends nothing.
	am.RunChecks(context.Background(), opts)
	if m := tg.messages(); len(m) != 1 {
		t.Errorf("sent again right after a proof: %d messages", len(m))
	}
}

// A delivered alert proves the path, so no scheduled message follows it.
func TestAlertCountsAsProof(t *testing.T) {
	hk := &hook{}
	hooks := httptest.NewServer(hk)
	defer hooks.Close()
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"since": map[string]time.Time{
		Target{"team", "webhook", 0}.key(): time.Now().Add(-2 * time.Hour)}})
	os.WriteFile(filepath.Join(dir, "delivery-checks.json"), b, 0o600)

	am, err := New(dir, "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(fmt.Sprintf(`
route: {receiver: team, group_by: [alertname], group_wait: 0s, group_interval: 1s}
receivers:
  - name: team
    webhook_configs: [{url: %q}]
`, hooks.URL)); err != nil {
		t.Fatal(err)
	}
	if err := am.Send(context.Background(), []Alert{{Labels: map[string]string{"alertname": "DiskFull"}, StartsAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if hk.find("firing DiskFull") != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	am.RunChecks(context.Background(), CheckOptions{SendInterval: time.Hour})
	if hk.find("firing "+CheckAlertName) != "" {
		t.Errorf("a check message followed a delivered alert: %v", hk.msgs)
	}
	if d := checkByKey(t, am, "team", "webhook"); d.Check.LastProof.IsZero() {
		t.Errorf("the alert did not count as proof: %+v", d.Check)
	}
}

// A scheduled message that failed is tried again within the hour, not at
// every check.
func TestFailedSendWaits(t *testing.T) {
	var calls atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "no", http.StatusBadRequest)
	}))
	defer broken.Close()
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"since": map[string]time.Time{
		Target{"team", "webhook", 0}.key(): time.Now().Add(-2 * time.Hour)}})
	os.WriteFile(filepath.Join(dir, "delivery-checks.json"), b, 0o600)
	am, err := New(dir, "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(fmt.Sprintf("route: {receiver: team}\nreceivers:\n  - name: team\n    webhook_configs: [{url: %q}]\n", broken.URL)); err != nil {
		t.Fatal(err)
	}
	opts := CheckOptions{SendInterval: time.Hour}
	am.RunChecks(context.Background(), opts)
	am.RunChecks(context.Background(), opts)
	if n := calls.Load(); n != 1 {
		t.Errorf("%d sends, want 1", n)
	}
	d := checkByKey(t, am, "team", "webhook")
	if d.Check.SendError == "" || d.Failed != 1 || d.Check.NextSend.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("after a failed send: %+v %+v", d, d.Check)
	}
}
