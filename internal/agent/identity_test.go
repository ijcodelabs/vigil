package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
)

// fakeSigner answers the identity API the way the server does, for one
// client name and one endpoint name.
type fakeSigner struct {
	ca *pki.Authority
	mu sync.Mutex
	// epoch of the client; signs counts signing requests.
	epoch int
	signs int
	names []string // the endpoint's approved names
}

func (f *fakeSigner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer agent-test-token" || r.Header.Get("X-Vigil-Agent") != "host-1" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/identity/state":
		json.NewEncoder(w).Encode(map[string]any{"epochs": map[string]int{"boop-chat": f.epoch},
			"endpoints": map[string][]string{"gateway": f.names}, "ca": string(f.ca.CAPEM())})
	case "/api/v1/identity/sign":
		var in struct{ Name, CSR string }
		json.NewDecoder(r.Body).Decode(&in)
		f.signs++
		var c []byte
		var err error
		if in.Name == "gateway" {
			c, err = f.ca.SignServing([]byte(in.CSR), f.names, time.Now())
		} else {
			c, err = f.ca.SignClient([]byte(in.CSR), in.Name, f.epoch, time.Now())
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"cert": string(c), "ca": string(f.ca.CAPEM()), "epoch": f.epoch})
	default:
		http.NotFound(w, r)
	}
}

func testAgent(t *testing.T, url string) *Agent {
	cfg := &config.Agent{AgentID: "host-1", SpoolDir: t.TempDir()}
	cfg.Server.URL, cfg.Server.Token = url, "agent-test-token"
	a := &Agent{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), http: http.DefaultClient, ready: make(chan struct{})}
	close(a.ready)
	return a
}

func TestIdentityRenews(t *testing.T) {
	ca, err := pki.Open(t.TempDir(), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSigner{ca: ca, epoch: 1, names: []string{"127.0.0.1"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := testAgent(t, srv.URL)
	out := t.TempDir()
	slot := config.IdentityClient{Name: "boop-chat", CertFile: filepath.Join(out, "chat.pem"),
		KeyFile: filepath.Join(out, "chat.key"), CAFile: filepath.Join(out, "ca.pem")}
	id := newIdentity(a, "gateway", []config.IdentityClient{slot})
	ctx := context.Background()

	step := func() {
		t.Helper()
		if err := id.pullState(ctx); err != nil {
			t.Fatal(err)
		}
		if err := id.renewEndpoint(ctx); err != nil {
			t.Fatal(err)
		}
		if err := id.renewClient(ctx, slot); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if f.signs != 2 {
		t.Fatalf("first round: %d signings", f.signs)
	}
	if _, err := tls.LoadX509KeyPair(slot.CertFile, slot.KeyFile); err != nil {
		t.Fatalf("client files: %v", err)
	}
	if st, _ := os.Stat(slot.KeyFile); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st.Mode().Perm())
	}
	if c, err := id.GetCertificate(nil); err != nil || c == nil {
		t.Fatalf("endpoint certificate: %v", err)
	}
	if id.ClientCAs() == nil || id.Epochs()["boop-chat"] != 1 {
		t.Errorf("state: %v", id.Epochs())
	}

	// Nothing due: nothing signed.
	step()
	if f.signs != 2 {
		t.Errorf("fresh certificates renewed: %d", f.signs)
	}
	// Revoked and approved again: a new epoch, the client renews.
	f.epoch = 2
	step()
	if f.signs != 3 {
		t.Errorf("new epoch: %d signings", f.signs)
	}
	b, _ := os.ReadFile(slot.CertFile)
	if due(b, 2, time.Now()) {
		t.Error("the renewed certificate is not at epoch 2")
	}
	// An admin adds a name to the endpoint: renewed with both.
	f.names = []string{"127.0.0.1", "gateway.internal"}
	step()
	if f.signs != 4 {
		t.Errorf("changed names: %d signings", f.signs)
	}
	if c, _ := id.GetCertificate(nil); !namesMatch(c.Certificate[0], f.names) {
		t.Error("the endpoint certificate does not have the new names")
	}
	step()
	if f.signs != 4 {
		t.Errorf("renewed again with the same names: %d", f.signs)
	}

	// A restart while the server is away keeps the endpoint and the state.
	a2 := testAgent(t, "http://127.0.0.1:1")
	a2.cfg.SpoolDir = a.cfg.SpoolDir
	id2 := newIdentity(a2, "gateway", nil)
	if c, _ := id2.GetCertificate(nil); c == nil || id2.Epochs()["boop-chat"] != 2 || id2.ClientCAs() == nil {
		t.Errorf("not kept across a restart: %v", id2.Epochs())
	}
}

func TestDue(t *testing.T) {
	ca, _ := pki.Open(t.TempDir(), []string{"localhost"})
	_, csr, _ := pki.NewKeyCSR()
	now := time.Now()
	c, _ := ca.SignClient(csr, "boop-chat", 3, now)
	for _, tc := range []struct {
		name string
		cert []byte
		want int
		at   time.Time
		due  bool
	}{
		{"fresh", c, 3, now, false},
		{"any epoch", c, 0, now, false},
		{"old epoch", c, 4, now, true},
		{"past half-life", c, 3, now.Add(pki.ClientValidity/2 + time.Hour), true},
		{"missing", nil, 3, now, true},
		{"garbage", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), 3, now, true},
	} {
		if got := due(tc.cert, tc.want, tc.at); got != tc.due {
			t.Errorf("%s: due %v", tc.name, got)
		}
	}
}

func TestWriteAtomicRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	os.WriteFile(target, []byte("keep"), 0o600)
	link := filepath.Join(dir, "cert.pem")
	os.Symlink(target, link)
	if err := writeAtomic(link, []byte("new"), 0o644, nil, nil, false); err == nil {
		t.Error("wrote through a symbolic link")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("target changed: %q", b)
	}
	// A directory that is a link, or missing, is refused too.
	os.Symlink(t.TempDir(), filepath.Join(dir, "linkdir"))
	if err := writeAtomic(filepath.Join(dir, "linkdir", "c.pem"), []byte("x"), 0o644, nil, nil, false); err == nil {
		t.Error("wrote into a linked directory")
	}
	if err := writeAtomic(filepath.Join(dir, "none", "c.pem"), []byte("x"), 0o644, nil, nil, false); err == nil {
		t.Error("made a directory it was not asked to")
	}
	p := filepath.Join(dir, "ok.pem")
	if err := writeAtomic(p, []byte("x"), 0o640, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}
