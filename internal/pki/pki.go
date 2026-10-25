// Package pki gives the server its own small certificate authority, so
// traffic can be encrypted without anyone having to buy or distribute
// certificates, and lets clients trust it by a fingerprint.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 90 * 24 * time.Hour
	// The server's certificate is replaced, in memory, when it has less
	// than this left. No restart and no agent change is needed: agents
	// trust the authority, not the certificate.
	leafRenewBefore = 30 * 24 * time.Hour
)

// Authority is the server's certificate authority and the certificate it
// currently serves with.
type Authority struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	pem   []byte
	names []string

	mu   sync.Mutex
	leaf *tls.Certificate
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// Open loads the authority kept in dir, creating it the first time. names
// are the host names and IP addresses the server is reached by.
func Open(dir string, names []string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certFile, keyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	a := &Authority{names: names}
	certPEM, err1 := os.ReadFile(certFile)
	keyPEM, err2 := os.ReadFile(keyFile)
	switch {
	case err1 == nil && err2 == nil:
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb == nil || kb == nil {
			return nil, fmt.Errorf("%s or %s is not PEM", certFile, keyFile)
		}
		var err error
		if a.cert, err = x509.ParseCertificate(cb.Bytes); err != nil {
			return nil, err
		}
		if a.key, err = x509.ParseECPrivateKey(kb.Bytes); err != nil {
			return nil, err
		}
		a.pem = certPEM
	case errors.Is(err1, os.ErrNotExist) && errors.Is(err2, os.ErrNotExist):
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: "vigil certificate authority"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(caValidity),
			IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return nil, err
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		a.cert, _ = x509.ParseCertificate(der)
		a.key = key
		a.pem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(certFile, a.pem, 0o644); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("certificate authority in %s is incomplete: %v %v", dir, err1, err2)
	}
	if _, err := a.GetCertificate(nil); err != nil {
		return nil, err
	}
	return a, nil
}

// CAPEM is the authority's certificate, for clients to trust.
func (a *Authority) CAPEM() []byte { return a.pem }

// Pin is the fingerprint clients are given to recognise the authority.
func (a *Authority) Pin() string { return pinOf(a.cert.Raw) }

func pinOf(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Pin returns the fingerprint of a PEM-encoded certificate.
func Pin(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not a PEM certificate")
	}
	return pinOf(block.Bytes), nil
}

var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// maxNames bounds how many names the certificate collects from clients.
const maxNames = 64

// GetCertificate is for tls.Config: it returns the server's certificate,
// issuing a new one when the current one is close to expiring.
//
// The name or address a client asks for is added to the certificate. That
// gives nothing away: this authority signs only this server's keys, so a
// client that trusts it is talking to this server whatever name it used.
// It saves listing every address of the server in its config.
func (a *Authority) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fresh := true
	if hello != nil {
		asked := []string{hello.ServerName}
		if hello.Conn != nil {
			if host, _, err := net.SplitHostPort(hello.Conn.LocalAddr().String()); err == nil {
				asked = append(asked, host)
			}
		}
		for _, n := range asked {
			n, _, _ = strings.Cut(n, "%") // an IPv6 zone is not part of the address
			ip := net.ParseIP(n)
			if n == "" || n == "localhost" || slices.Contains(a.names, n) || len(a.names) >= maxNames ||
				(ip == nil && !hostNameRe.MatchString(n)) || (ip != nil && ip.IsLoopback()) {
				continue
			}
			a.names, fresh = append(a.names, n), false
		}
	}
	if fresh && a.leaf != nil && time.Until(a.leaf.Leaf.NotAfter) > leafRenewBefore {
		return a.leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "vigil server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(leafValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, n := range a.names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" && n != "localhost" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	a.leaf = &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: leaf}
	return a.leaf, nil
}

// ClientOptions say how a client decides to trust the server.
type ClientOptions struct {
	ServerURL string
	CAFile    string // a certificate file to trust, or
	CAPin     string // the fingerprint of the server's authority
	CacheFile string // where a pinned authority's certificate is kept once fetched
	Insecure  bool   // skip verification; for tests only
}

// ClientTLS returns the TLS settings for talking to the server. With a pin,
// the authority's certificate is fetched from the server once, accepted only
// if its fingerprint is the pinned one, and kept in CacheFile.
func ClientTLS(o ClientOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.Insecure}
	var caPEM []byte
	switch {
	case o.CAFile != "":
		b, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, err
		}
		caPEM = b
	case o.CAPin != "":
		want := strings.ToLower(strings.TrimSpace(o.CAPin))
		if b, err := os.ReadFile(o.CacheFile); err == nil {
			if got, _ := Pin(b); got == want {
				caPEM = b
			}
		}
		if caPEM == nil {
			b, err := fetchCA(o.ServerURL)
			if err != nil {
				return nil, fmt.Errorf("fetching the server's certificate authority: %w", err)
			}
			got, err := Pin(b)
			if err != nil {
				return nil, err
			}
			if got != want {
				return nil, fmt.Errorf("the server's certificate authority has fingerprint %s, not the expected %s: "+
					"this is not the server the fingerprint was issued for, or someone is in between", got, want)
			}
			caPEM = b
			if o.CacheFile != "" {
				os.MkdirAll(filepath.Dir(o.CacheFile), 0o750)
				os.WriteFile(o.CacheFile, b, 0o644)
			}
		}
	default:
		return cfg, nil // the system's trusted authorities
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificate found in the certificate authority file")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// fetchCA downloads the authority's certificate without verifying the
// connection. That is safe only because the caller then checks the
// fingerprint.
func fetchCA(serverURL string) ([]byte, error) {
	hc := &http.Client{Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Get(strings.TrimRight(serverURL, "/") + "/api/v1/ca")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d (does the server use its own certificate authority?)", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// HTTPClient is an HTTP client for the command-line tools (enrol, apply)
// that trusts the server the way ClientTLS describes.
func HTTPClient(o ClientOptions) (*http.Client, error) {
	tc, err := ClientTLS(o)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}
