package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
)

// identity keeps certificates from the server's authority
// (memory/context/identity.md):
//
//   - for programs on this host that send to an OTLP endpoint (the
//     identity.clients of the agent's own file): a key and a client
//     certificate written to the files the host's owner chose;
//   - for this agent's OTLP endpoint, when otlp.tls.from_server names an
//     endpoint identity: its serving certificate, kept in the spool;
//   - which clients are current, at which epoch, and the authority's
//     certificate, for an endpoint that checks client certificates.
//
// A certificate is renewed at half its life, when its epoch is no longer
// the current one, and when it is missing. The key is made here every time;
// the server signs and never sees it. When the server cannot be reached,
// everything keeps what it has.
type identity struct {
	a        *Agent
	clients  []config.IdentityClient
	endpoint string // endpoint identity name, or ""

	mu      sync.Mutex
	serving *tls.Certificate
	epochs  map[string]int
	names   []string // the endpoint's approved names, from the last state
	pool    *x509.CertPool
	stateAt time.Time
	failing bool
}

// newIdentity keeps the given client slots, or an endpoint's certificate
// and the client state, or both. The agent runs one for its local clients
// for as long as it runs, and one for its OTLP endpoint with each
// collection config.
func newIdentity(a *Agent, endpoint string, clients []config.IdentityClient) *identity {
	id := &identity{a: a, clients: clients, endpoint: endpoint}
	id.loadSaved()
	return id
}

func (id *identity) dir() string { return filepath.Join(id.a.cfg.SpoolDir, "identity") }

// loadSaved takes what was kept from the last run, so a restart while the
// server is away keeps serving and checking.
func (id *identity) loadSaved() {
	if id.endpoint != "" {
		if c, err := tls.LoadX509KeyPair(filepath.Join(id.dir(), "endpoint.pem"), filepath.Join(id.dir(), "endpoint.key")); err == nil {
			id.serving = &c
		}
	}
	if b, err := os.ReadFile(filepath.Join(id.dir(), "state.json")); err == nil {
		var st identityState
		if json.Unmarshal(b, &st) == nil {
			id.setState(st)
		}
	}
}

type identityState struct {
	Epochs map[string]int `json:"epochs"`
	// Endpoints: the approved names of the endpoints this agent may hold.
	Endpoints map[string][]string `json:"endpoints,omitempty"`
	CA        string              `json:"ca"`
}

func (id *identity) setState(st identityState) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(st.CA)) {
		pool = nil
	}
	id.mu.Lock()
	id.epochs, id.pool, id.stateAt = st.Epochs, pool, time.Now()
	if names, ok := st.Endpoints[id.endpoint]; ok {
		id.names = names
	}
	id.mu.Unlock()
}

// GetCertificate serves the endpoint's certificate.
func (id *identity) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.serving == nil {
		return nil, errors.New("the endpoint has no certificate from the server yet")
	}
	return id.serving, nil
}

// ClientCAs is the server's authority, for checking clients.
func (id *identity) ClientCAs() *x509.CertPool {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.pool
}

// Epochs are the current clients and their epochs.
func (id *identity) Epochs() map[string]int {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.epochs
}

// run keeps everything fresh until ctx ends.
func (id *identity) run(ctx context.Context, checkClients bool) {
	select {
	case <-ctx.Done():
		return
	case <-id.a.ready:
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		ok := true
		if checkClients || len(id.clients) > 0 || id.endpoint != "" {
			if err := id.pullState(ctx); err != nil {
				ok = false
				id.warn("identity state", err)
			}
		}
		if id.endpoint != "" {
			if err := id.renewEndpoint(ctx); err != nil {
				ok = false
				id.warn("endpoint certificate", err)
			}
		}
		for _, c := range id.clients {
			if err := id.renewClient(ctx, c); err != nil {
				ok = false
				id.warn("client certificate "+c.Name, err)
			}
		}
		if ok {
			id.failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (id *identity) warn(what string, err error) {
	if !id.failing && err != nil {
		id.a.log.Warn(what+": keeping what is there", "err", err)
		id.failing = true
	}
}

func (id *identity) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(id.a.cfg.Server.URL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+id.a.cfg.Server.Token)
	req.Header.Set("X-Vigil-Agent", id.a.cfg.AgentID)
	resp, err := id.a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

func (id *identity) pullState(ctx context.Context) error {
	var st identityState
	if err := id.call(ctx, http.MethodGet, "/api/v1/identity/state", nil, &st); err != nil {
		return err
	}
	id.setState(st)
	b, _ := json.Marshal(st)
	return writeAtomic(filepath.Join(id.dir(), "state.json"), b, 0o600, nil, nil, true)
}

type signed struct {
	Cert  string `json:"cert"`
	CA    string `json:"ca"`
	Epoch int    `json:"epoch"`
}

func (id *identity) sign(ctx context.Context, name string) (keyPEM []byte, s signed, err error) {
	keyPEM, csr, err := pki.NewKeyCSR()
	if err != nil {
		return nil, s, err
	}
	err = id.call(ctx, http.MethodPost, "/api/v1/identity/sign", map[string]string{"name": name, "csr": string(csr)}, &s)
	return keyPEM, s, err
}

// due says whether a certificate needs renewing: missing, unreadable, past
// half its life, or of an epoch that is no longer current (want 0: any).
func due(certPEM []byte, want int, now time.Time) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return true
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return true
	}
	if now.After(c.NotBefore.Add(c.NotAfter.Sub(c.NotBefore) / 2)) {
		return true
	}
	if want > 0 {
		if _, epoch, err := pki.ClientOf(c); err != nil || epoch != want {
			return true
		}
	}
	return false
}

func (id *identity) renewEndpoint(ctx context.Context) error {
	id.mu.Lock()
	cur, names := id.serving, id.names
	id.mu.Unlock()
	if cur != nil && !due(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cur.Certificate[0]}), 0, time.Now()) &&
		(names == nil || namesMatch(cur.Certificate[0], names)) {
		return nil
	}
	keyPEM, s, err := id.sign(ctx, id.endpoint)
	if err != nil {
		return err
	}
	chain := []byte(s.Cert + s.CA)
	c, err := tls.X509KeyPair(chain, keyPEM)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(id.dir(), "endpoint.key"), keyPEM, 0o600, nil, nil, true); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(id.dir(), "endpoint.pem"), chain, 0o644, nil, nil, true); err != nil {
		return err
	}
	id.mu.Lock()
	id.serving = &c
	id.mu.Unlock()
	id.a.log.Info("endpoint certificate renewed", "identity", id.endpoint)
	return nil
}

// namesMatch says whether a serving certificate is for exactly these host
// names and addresses (an admin may have changed them).
func namesMatch(der []byte, names []string) bool {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return false
	}
	have := map[string]bool{}
	for _, n := range c.DNSNames {
		have[n] = true
	}
	for _, ip := range c.IPAddresses {
		have[ip.String()] = true
	}
	want := map[string]bool{}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			n = ip.String()
		}
		want[n] = true
	}
	return maps.Equal(have, want)
}

func (id *identity) renewClient(ctx context.Context, c config.IdentityClient) error {
	want := id.Epochs()[c.Name]
	if cur, err := os.ReadFile(c.CertFile); err == nil && !due(cur, want, time.Now()) {
		return nil
	}
	keyPEM, s, err := id.sign(ctx, c.Name)
	if err != nil {
		return err
	}
	// The key first: a program that sees the new certificate finds its key.
	keyMode := os.FileMode(0o600)
	if c.Group != nil && *c.Group >= 0 {
		keyMode = 0o640
	}
	if err := writeAtomic(c.KeyFile, keyPEM, keyMode, c.Owner, c.Group, false); err != nil {
		return err
	}
	if err := writeAtomic(c.CertFile, []byte(s.Cert), 0o644, c.Owner, c.Group, false); err != nil {
		return err
	}
	if c.CAFile != "" {
		if err := writeAtomic(c.CAFile, []byte(s.CA), 0o644, c.Owner, c.Group, false); err != nil {
			return err
		}
	}
	id.a.log.Info("client certificate renewed", "client", c.Name, "epoch", s.Epoch)
	return nil
}

// writeAtomic writes a file through a new file in the same directory and a
// rename, so a reader never sees half of it. The directory must exist
// (mkdir: made, for the agent's own spool) and the target must not be a
// symbolic link: credentials are never written through one.
func writeAtomic(path string, data []byte, mode os.FileMode, owner, group *int, mkdir bool) error {
	dir := filepath.Dir(path)
	if mkdir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s: the directory must exist and not be a link", dir)
	}
	if st, err := os.Lstat(path); err == nil && (st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular()) {
		return fmt.Errorf("%s is not a plain file: not written", path)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	uid, gid := -1, -1
	if owner != nil {
		uid = *owner
	}
	if group != nil {
		gid = *group
	}
	if uid >= 0 || gid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
