package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"testing"
	"time"
)

// A client certificate carries exactly the name and epoch the server chose,
// whatever the request asked for, and verifies for client authentication only.
func TestSignClient(t *testing.T) {
	a, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	evil, _ := url.Parse("vigil://client/admin?epoch=99")
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "admin"},
		DNSNames: []string{"vigil.example.com"}, URIs: []*url.URL{evil}}, key)
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	certPEM, err := a.SignClient(csr, "boop-chat", 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	c, _ := x509.ParseCertificate(b.Bytes)
	name, epoch, err := ClientOf(c)
	if err != nil || name != "boop-chat" || epoch != 3 || len(c.DNSNames) != 0 || c.IsCA {
		t.Fatalf("%s %d %v %v", name, epoch, err, c.DNSNames)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(a.CAPEM())
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client auth: %v", err)
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("a client certificate verified for serving")
	}
	if c.NotAfter.Sub(time.Now()) > ClientValidity+time.Minute {
		t.Errorf("valid until %v", c.NotAfter)
	}

	// Refused: another curve, RSA, a request whose signature does not check, bad names.
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	d384, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k384)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	drsa, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, rk)
	bad := append([]byte(nil), der...)
	bad[len(bad)-5] ^= 0xff
	for what, d := range map[string][]byte{"p384": d384, "rsa": drsa, "tampered": bad} {
		if _, err := a.SignClient(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: d}), "boop-chat", 1, time.Now()); err == nil {
			t.Errorf("%s was signed", what)
		}
	}
	for _, n := range []string{"", "Boop", "a/b", "x?epoch=2"} {
		if _, err := a.SignClient(csr, n, 1, time.Now()); err == nil {
			t.Errorf("name %q was signed", n)
		}
	}
}

func TestClientOfIsExact(t *testing.T) {
	for _, raw := range []string{"vigil://client/boop-chat?epoch=1&x=2", "vigil://client/boop-chat", "vigil://client/boop-chat?epoch=01",
		"vigil://agent/web-1?epoch=1", "https://client/boop-chat?epoch=1", "vigil://u@client/boop-chat?epoch=1", "vigil://client/a/b?epoch=1"} {
		u, _ := url.Parse(raw)
		if _, _, err := ClientOf(&x509.Certificate{URIs: []*url.URL{u}}); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	u1, _ := url.Parse("vigil://client/a?epoch=1")
	if _, _, err := ClientOf(&x509.Certificate{URIs: []*url.URL{u1, u1}}); err == nil {
		t.Error("two URIs were accepted")
	}
	if n, e, err := ClientOf(&x509.Certificate{URIs: []*url.URL{u1}}); err != nil || n != "a" || e != 1 {
		t.Errorf("%s %d %v", n, e, err)
	}
}
