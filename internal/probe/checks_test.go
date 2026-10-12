package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

// A certificate for 127.0.0.1 that is valid until notAfter.
func certUntil(t *testing.T, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: notAfter.Add(-30 * 24 * time.Hour), NotAfter: notAfter,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func tlsListener(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// A TLS probe reports the time left on the certificate also when it refuses
// the certificate: an expired one must show up as expired, not as nothing.
func TestTLS(t *testing.T) {
	good := tlsListener(t, certUntil(t, time.Now().Add(10*24*time.Hour)))
	v := values(config.Probe{Name: "mail", TLS: good, TLSInsecure: true})
	if days := v["probe_tls_cert_expiry_seconds"] / 86400; v["probe_success"] != 1 || days < 9.9 || days > 10.1 {
		t.Errorf("valid for ten days, not verified: %v", v)
	}
	// Self-signed: refused when verified, and still measured.
	v = values(config.Probe{Name: "mail", TLS: good})
	if v["probe_success"] != 0 || v["probe_tls_cert_expiry_seconds"] <= 0 {
		t.Errorf("untrusted certificate: %v", v)
	}

	expired := tlsListener(t, certUntil(t, time.Now().Add(-48*time.Hour)))
	v = values(config.Probe{Name: "old", TLS: expired})
	if v["probe_success"] != 0 || v["probe_tls_cert_expiry_seconds"] > -47*3600 {
		t.Errorf("expired two days ago: %v", v)
	}

	if v = values(config.Probe{Name: "none", TLS: "127.0.0.1:1"}); v["probe_success"] != 0 {
		t.Errorf("nothing listening: %v", v)
	}
	if _, has := v["probe_tls_cert_expiry_seconds"]; has {
		t.Errorf("no certificate was seen, none should be reported: %v", v)
	}
}

func TestExpectBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok","db":"up"}`)
	}))
	defer srv.Close()
	if v := values(config.Probe{Name: "api", HTTP: srv.URL, ExpectBody: `"db":"up"`}); v["probe_success"] != 1 {
		t.Errorf("body matches: %v", v)
	}
	if v := values(config.Probe{Name: "api", HTTP: srv.URL, ExpectBody: `"db":"down"`}); v["probe_success"] != 0 || v["probe_http_status_code"] != 200 {
		t.Errorf("body does not match: %v", v)
	}
}

// What is checked becomes a label, without the secrets a URL may carry.
func TestLabels(t *testing.T) {
	samples := Check(context.Background(), config.Probe{Name: "x", HTTP: "http://user:secret@127.0.0.1:1/health?token=abc", Timeout: config.Duration(time.Second)})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.Labels["type"] != "http" || s.Labels["target"] != "http://127.0.0.1:1/health" || strings.Contains(s.Labels["target"], "secret") {
			t.Errorf("%s: labels %v", s.Name, s.Labels)
		}
	}
}

func TestPing(t *testing.T) {
	if c, _, err := listenICMP(true); err != nil {
		t.Skipf("this user may not open an ICMP socket: %v", err)
	} else {
		c.Close()
	}
	v := values(config.Probe{Name: "lo", Ping: "127.0.0.1", Timeout: config.Duration(3 * time.Second)})
	if v["probe_success"] != 1 || v["probe_packet_loss_ratio"] != 0 || v["probe_duration_seconds"] <= 0 || v["probe_duration_seconds"] > 0.5 {
		t.Errorf("ping to this host: %v", v)
	}
	// 192.0.2.0/24 is reserved for documentation: nothing answers there.
	start := time.Now()
	v = values(config.Probe{Name: "nobody", Ping: "192.0.2.1", Timeout: config.Duration(1500 * time.Millisecond)})
	if v["probe_success"] != 0 || v["probe_packet_loss_ratio"] != 1 || time.Since(start) > 3*time.Second {
		t.Errorf("ping to nobody: %v after %v", v, time.Since(start))
	}
	if v = values(config.Probe{Name: "noname", Ping: "does-not-exist.invalid", Timeout: config.Duration(2 * time.Second)}); v["probe_success"] != 0 {
		t.Errorf("a name that does not resolve: %v", v)
	}
}
