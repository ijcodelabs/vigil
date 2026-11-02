package server_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
	"github.com/ijcodelabs/vigil/internal/server"
)

func TestIdentities(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret", HeartbeatTimeout: config.Duration(time.Minute)}
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = stores.URL, stores.URL
	cfg.TLS.SelfSigned = true
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	central := httptest.NewServer(srv.Handler())
	defer func() { central.Close(); srv.Close() }()
	c := newClient(t, central.URL)
	c.csrf = false
	admin := func(method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	_, body := admin("POST", "/api/v1/agents/gw-1/token", "")
	var tok struct{ Token string }
	json.Unmarshal([]byte(body), &tok)
	_, body = admin("POST", "/api/v1/agents/web-1/token", "")
	var other struct{ Token string }
	json.Unmarshal([]byte(body), &other)
	asAgent := func(agent, token, method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer "+token, "X-Vigil-Agent", agent)
		return code, out
	}
	_, csr, _ := pki.NewKeyCSR()
	signReq := func(name string) string {
		b, _ := json.Marshal(map[string]string{"name": name, "csr": string(csr)})
		return string(b)
	}

	for body, want := range map[string]int{`{"agents":"gw-1"}`: 200, `{"agents":""}`: 400, `{"agents":"(","kind":"client"}`: 400,
		`{"agents":"gw-1","kind":"endpoint"}`: 400, `{"agents":"gw-1","kind":"x"}`: 400} {
		if code, out := admin("PUT", "/api/v1/identities/boop-chat", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, _ := admin("PUT", "/api/v1/identities/Bad%20Name", `{"agents":"gw-1"}`); code != 400 {
		t.Errorf("bad name: %d", code)
	}

	// The agent with its own token gets a certificate; the shared ingest
	// token, another agent and an unknown name get none.
	code, body := asAgent("gw-1", tok.Token, "POST", "/api/v1/identity/sign", signReq("boop-chat"))
	var signed struct {
		Cert, CA string
		Epoch    int
	}
	if json.Unmarshal([]byte(body), &signed); code != 200 || signed.Epoch != 1 || !strings.Contains(signed.CA, "BEGIN CERTIFICATE") {
		t.Fatalf("sign: %d %s", code, body)
	}
	b, _ := pem.Decode([]byte(signed.Cert))
	cert, _ := x509.ParseCertificate(b.Bytes)
	if name, epoch, err := pki.ClientOf(cert); name != "boop-chat" || epoch != 1 || err != nil {
		t.Errorf("certificate: %s %d %v", name, epoch, err)
	}
	for what, call := range map[string][2]string{"ingest token": {"gw-1", "ingest"}, "another agent": {"web-1", other.Token},
		"someone else's token": {"gw-1", other.Token}} {
		if code, _ := asAgent(call[0], call[1], "POST", "/api/v1/identity/sign", signReq("boop-chat")); code != 401 && code != 403 {
			t.Errorf("%s: %d", what, code)
		}
	}
	if code, _ := asAgent("gw-1", tok.Token, "POST", "/api/v1/identity/sign", signReq("someone")); code != 403 {
		t.Errorf("an unknown name: %d", code)
	}

	code, body = asAgent("gw-1", tok.Token, "GET", "/api/v1/identity/state", "")
	if code != 200 || !strings.Contains(body, `"boop-chat":1`) {
		t.Errorf("state: %d %s", code, body)
	}

	// Revoked: no new certificates and not current; approved again at a new epoch.
	if code, body := admin("POST", "/api/v1/identities/boop-chat/revoke", ""); code != 200 || !strings.Contains(body, `"epoch":2`) || !strings.Contains(body, `"revoked":true`) {
		t.Errorf("revoke: %d %s", code, body)
	}
	if code, _ := asAgent("gw-1", tok.Token, "POST", "/api/v1/identity/sign", signReq("boop-chat")); code != 403 {
		t.Errorf("a revoked name was signed: %d", code)
	}
	if _, body := asAgent("gw-1", tok.Token, "GET", "/api/v1/identity/state", ""); strings.Contains(body, "boop-chat") {
		t.Errorf("a revoked name is current: %s", body)
	}
	admin("PUT", "/api/v1/identities/boop-chat", `{"agents":"gw-1"}`)
	if _, body := asAgent("gw-1", tok.Token, "GET", "/api/v1/identity/state", ""); !strings.Contains(body, `"boop-chat":3`) {
		t.Errorf("approved again: %s", body)
	}

	// An endpoint certificate: only for the names an admin approved.
	if code, _ := admin("PUT", "/api/v1/identities/gw-otlp", `{"kind":"endpoint","agents":"gw-1","names":["192.0.2.54"]}`); code != 200 {
		t.Fatalf("endpoint: %d", code)
	}
	if _, body := asAgent("gw-1", tok.Token, "GET", "/api/v1/identity/state", ""); !strings.Contains(body, `"endpoints":{"gw-otlp":["192.0.2.54"]}`) {
		t.Errorf("own endpoint's names not in the state: %s", body)
	}
	if _, body := asAgent("web-1", other.Token, "GET", "/api/v1/identity/state", ""); strings.Contains(body, "gw-otlp") {
		t.Errorf("another agent's endpoint is in the state: %s", body)
	}
	code, body = asAgent("gw-1", tok.Token, "POST", "/api/v1/identity/sign", signReq("gw-otlp"))
	json.Unmarshal([]byte(body), &signed)
	b, _ = pem.Decode([]byte(signed.Cert))
	cert, _ = x509.ParseCertificate(b.Bytes)
	if code != 200 || len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "192.0.2.54" || len(cert.DNSNames) != 0 ||
		cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("endpoint certificate: %d %+v", code, cert)
	}
	code, body = admin("GET", "/api/v1/identities", "")
	if code != 200 || !strings.Contains(body, `"last_issued"`) || !strings.Contains(body, `"gw-otlp"`) {
		t.Errorf("list: %d %s", code, body)
	}
}
