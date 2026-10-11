package pki

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorityAndPinning(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir, []string{"vigil.example.com", "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Pin(), "sha256:") || len(a.Pin()) != 71 {
		t.Fatalf("pin: %q", a.Pin())
	}
	// Opening again finds the same authority; the fingerprint must not change over restarts.
	b, err := Open(dir, nil)
	if err != nil || b.Pin() != a.Pin() {
		t.Fatalf("reopened authority differs: %v", err)
	}
	leaf, _ := a.GetCertificate(nil)
	if got := strings.Join(leaf.Leaf.DNSNames, ","); got != "localhost,vigil.example.com" || len(leaf.Leaf.IPAddresses) != 3 {
		t.Errorf("certificate names: %s %v", got, leaf.Leaf.IPAddresses)
	}

	// A server using that authority, as the real one does.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ca", func(w http.ResponseWriter, _ *http.Request) { w.Write(a.CAPEM()) })
	mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "hi") })
	srv := httptest.NewUnstartedServer(mux)
	// The test server would add a certificate of its own next to
	// GetCertificate and prefer it for connections by IP address.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	srv.StartTLS()
	defer srv.Close()

	get := func(o ClientOptions) error {
		o.ServerURL = srv.URL
		cfg, err := ClientTLS(o)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}).Get(srv.URL + "/hello")
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	cache := filepath.Join(t.TempDir(), "ca.pem")
	if err := get(ClientOptions{CAPin: a.Pin(), CacheFile: cache}); err != nil {
		t.Errorf("with the right pin: %v", err)
	}
	if got, _ := Pin(mustRead(t, cache)); got != a.Pin() {
		t.Error("the authority's certificate was not cached")
	}
	if err := get(ClientOptions{CAFile: cache}); err != nil {
		t.Errorf("with the certificate file: %v", err)
	}
	// Without being told to trust it, a client refuses the server.
	if err := get(ClientOptions{}); err == nil {
		t.Error("an unknown authority was trusted")
	}
	// A wrong pin is refused, with a message that says what it means.
	wrong := "sha256:" + strings.Repeat("0", 64)
	if err := get(ClientOptions{CAPin: wrong, CacheFile: filepath.Join(t.TempDir(), "x.pem")}); err == nil || !strings.Contains(err.Error(), "not the expected") {
		t.Errorf("wrong pin: %v", err)
	}
	// A cached certificate that does not match the pin is not used.
	other, _ := Open(t.TempDir(), nil)
	if err := get(ClientOptions{CAPin: other.Pin(), CacheFile: cache}); err == nil {
		t.Error("a cached certificate of another authority was accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	cfg, err := ClientTLS(ClientOptions{CAFile: path})
	if err != nil || cfg.RootCAs == nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	b, _ := readFile(path)
	return b
}
