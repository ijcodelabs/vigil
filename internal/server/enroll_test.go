package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/agent"
	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
	"github.com/ijcodelabs/vigil/internal/server"
)

// A server with its own certificate authority, and a host that knows only an
// enrolment token and the authority's fingerprint: it ends up sending data
// over a verified connection with a token of its own.
func TestEnrolOverTLS(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeStore{}
	vm := httptest.NewServer(store)
	defer vm.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	base := "https://" + addr

	dataDir := t.TempDir()
	scfg := &config.Server{Listen: addr, DataDir: dataDir, AdminToken: "admin-token-secret", ExternalURL: base,
		HeartbeatTimeout: config.Duration(time.Minute), Interval: config.Duration(time.Hour)}
	scfg.TLS.SelfSigned = true
	scfg.Storage.MetricsURL, scfg.Storage.LogsURL = vm.URL, vm.URL
	srv, err := server.New(scfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)

	// Someone setting up a host reads the fingerprint from the UI; here it
	// comes from the certificate the server wrote.
	caPEM, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := pki.Pin(caPEM)

	tc, err := pki.ClientTLS(pki.ClientOptions{ServerURL: base, CAFile: filepath.Join(dataDir, "pki", "ca.pem")})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	call := func(method, path, token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	waitFor(t, "the server to listen", func() bool {
		c, err := tls.Dial("tcp", addr, tc)
		if err == nil {
			c.Close()
		}
		return err == nil
	})

	// The fingerprint is offered to signed-in users, the certificate to anyone.
	code, body := call("GET", "/api/v1/server-info", "admin-token-secret", "")
	var info struct {
		URL, CAPin string
		Encrypted  bool
	}
	json.Unmarshal([]byte(strings.ReplaceAll(body, "ca_pin", "CAPin")), &info)
	if code != 200 || info.CAPin != pin || info.URL != base || !info.Encrypted {
		t.Fatalf("server-info: %d %s", code, body)
	}
	if code, _ := call("GET", "/api/v1/server-info", "", ""); code != http.StatusUnauthorized {
		t.Errorf("server-info without signing in: %d", code)
	}
	if code, body := call("GET", "/api/v1/ca", "", ""); code != 200 || body != strings.TrimSpace(string(caPEM)) {
		t.Errorf("ca: %d", code)
	}
	// A client that does not know the authority refuses the server.
	if _, err := (&http.Client{Transport: &http.Transport{}}).Get(base + "/healthz"); err == nil {
		t.Error("an unknown authority was trusted")
	}

	// Enrolment tokens: only admins make them, and only the hash is kept.
	if code, _ := call("POST", "/api/v1/fleet/enrollment", "", `{"name":"x"}`); code != http.StatusUnauthorized {
		t.Errorf("anonymous creates an enrolment token: %d", code)
	}
	if code, _ := call("POST", "/api/v1/fleet/enrollment", "admin-token-secret", `{"name":""}`); code != http.StatusBadRequest {
		t.Errorf("token without a name: %d", code)
	}
	code, body = call("POST", "/api/v1/fleet/enrollment", "admin-token-secret", `{"name":"rack 12","valid_hours":1,"max_uses":2}`)
	var made struct{ ID, Token string }
	json.Unmarshal([]byte(body), &made)
	if code != 200 || !strings.HasPrefix(made.Token, "vge_") {
		t.Fatalf("create: %d %s", code, body)
	}
	if db, _ := os.ReadFile(filepath.Join(dataDir, "vigil.db")); strings.Contains(string(db), made.Token) {
		t.Error("the enrolment token is stored in the clear")
	}

	enrol := func(token, id string) (int, string) {
		b, _ := json.Marshal(map[string]string{"token": token, "agent_id": id})
		return call("POST", "/api/v1/enroll", "", string(b))
	}
	if code, _ := enrol("vge_wrong", "web-01"); code != http.StatusForbidden {
		t.Errorf("unknown token: %d", code)
	}
	if code, _ := enrol(made.Token, "bad id"); code != http.StatusBadRequest {
		t.Errorf("bad agent id: %d", code)
	}
	// An enrolment token is not an ingest token.
	if code, _ := call("POST", "/api/v1/logs", made.Token, ""); code != http.StatusUnauthorized {
		t.Errorf("enrolment token used to send data: %d", code)
	}

	// The agent: no token of its own, just the enrolment token and the pin.
	on, off := true, false
	spool := t.TempDir()
	acfg := &config.Agent{AgentID: "web-01", SpoolDir: spool, SpoolMaxBytes: 1 << 20, RemoteConfig: &off}
	acfg.Interval, acfg.HostMetrics = config.Duration(100*time.Millisecond), &on
	acfg.Docker.Logs, acfg.Docker.Metrics = &off, &off
	acfg.Server.URL, acfg.Server.EnrollToken, acfg.Server.CAPin = base, made.Token, pin
	a, err := agent.New(acfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	actx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { a.Run(actx); close(agentDone) }()
	waitFor(t, "metrics from the enrolled agent", func() bool { _, ok := store.find("up", "web-01"); return ok || hasAgent(store, "web-01") })
	saved, err := os.ReadFile(filepath.Join(spool, "agent-token"))
	if err != nil || !strings.HasPrefix(string(saved), "vgl_") {
		t.Fatalf("saved token: %q %v", saved, err)
	}
	if fi, _ := os.Stat(filepath.Join(spool, "agent-token")); fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode: %v", fi.Mode())
	}
	if cached, _ := os.ReadFile(filepath.Join(spool, "ca.pem")); string(cached) != string(caPEM) {
		t.Error("the authority's certificate was not cached")
	}
	stopAgent()
	<-agentDone

	// Starting again uses the saved token: no second use is counted.
	a, _ = agent.New(acfg, "test", log)
	acfg.Server.Token = ""
	actx, stopAgent = context.WithCancel(ctx)
	agentDone = make(chan struct{})
	store.mu.Lock()
	store.samples = nil
	store.mu.Unlock()
	go func() { a.Run(actx); close(agentDone) }()
	waitFor(t, "metrics after a restart", func() bool { return hasAgent(store, "web-01") })
	stopAgent()
	<-agentDone
	_, body = call("GET", "/api/v1/fleet/enrollment", "admin-token-secret", "")
	var list []server.EnrollToken
	json.Unmarshal([]byte(body), &list)
	if len(list) != 1 || list[0].Uses != 1 || list[0].MaxUses != 2 || list[0].Name != "rack 12" || list[0].Expires.IsZero() {
		t.Fatalf("list: %s", body)
	}

	// The same name cannot be taken twice; that would let anyone holding
	// the enrolment token replace a host.
	if code, body := enrol(made.Token, "web-01"); code != http.StatusForbidden || !strings.Contains(body, "already enrolled") {
		t.Errorf("second enrolment of the same id: %d %s", code, body)
	}
	// The token one host got does not work for another host's name.
	tok := strings.TrimSpace(string(saved))
	req, _ := http.NewRequest("POST", base+"/api/v1/logs", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Vigil-Agent", "db-01")
	if resp, err := hc.Do(req); err != nil || resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("agent token used under another name: %v %v", resp, err)
	}
	// Uses run out, and a revoked token stops working.
	if code, _ := enrol(made.Token, "web-02"); code != 200 {
		t.Errorf("second use: %d", code)
	}
	if code, body := enrol(made.Token, "web-03"); code != http.StatusForbidden || !strings.Contains(body, "as often as it allows") {
		t.Errorf("third use: %d %s", code, body)
	}
	_, body = call("POST", "/api/v1/fleet/enrollment", "admin-token-secret", `{"name":"forever"}`)
	json.Unmarshal([]byte(body), &made)
	if code, _ := call("DELETE", "/api/v1/fleet/enrollment/"+made.ID, "admin-token-secret", ""); code != http.StatusNoContent {
		t.Errorf("revoke: %d", code)
	}
	if code, _ := enrol(made.Token, "web-04"); code != http.StatusForbidden {
		t.Errorf("revoked token: %d", code)
	}

	// A wrong fingerprint is refused before anything is sent.
	if _, err := pki.ClientTLS(pki.ClientOptions{ServerURL: base, CAPin: "sha256:" + strings.Repeat("0", 64)}); err == nil ||
		!strings.Contains(err.Error(), "not the expected") {
		t.Errorf("wrong pin: %v", err)
	}
}

func hasAgent(f *fakeStore, agent string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.samples {
		if s.Labels["agent"] == agent {
			return true
		}
	}
	return false
}
