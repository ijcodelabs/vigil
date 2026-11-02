package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Client and serving certificates for what talks to an agent's OTLP
// endpoint (memory/context/identity.md). The authority signs a request
// (CSR) whose key the asker made; everything the request asks for besides
// its key is ignored and the certificate is built here: one URI naming the
// client and its epoch, client authentication only, never a CA.

// Validity of the certificates signed for clients and endpoints. They are
// renewed at half-life, so a server that is away for a few days stops
// nothing.
const (
	ClientValidity = 7 * 24 * time.Hour
	skew           = 5 * time.Minute
)

// NameRe is what a client name may be.
var NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ClientURI names a client in its certificate: vigil://client/<name>?epoch=<n>.
func ClientURI(name string, epoch int) *url.URL {
	return &url.URL{Scheme: "vigil", Host: "client", Path: "/" + name, RawQuery: "epoch=" + strconv.Itoa(epoch)}
}

// ClientOf reads a certificate's client name and epoch, exactly: one URI
// SAN of the form ClientURI makes and nothing else that names it.
func ClientOf(c *x509.Certificate) (name string, epoch int, err error) {
	if len(c.URIs) != 1 || len(c.DNSNames) != 0 || len(c.IPAddresses) != 0 || len(c.EmailAddresses) != 0 {
		return "", 0, errors.New("pki: a client certificate names exactly one vigil client")
	}
	u := c.URIs[0]
	if u.Scheme != "vigil" || u.Host != "client" || u.User != nil || u.Fragment != "" || len(u.Path) < 2 {
		return "", 0, errors.New("pki: not a vigil client URI")
	}
	name = u.Path[1:]
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) != 1 || len(q["epoch"]) != 1 || !NameRe.MatchString(name) {
		return "", 0, errors.New("pki: malformed vigil client URI")
	}
	if epoch, err = strconv.Atoi(q["epoch"][0]); err != nil || epoch < 1 || strconv.Itoa(epoch) != q["epoch"][0] {
		return "", 0, errors.New("pki: malformed epoch")
	}
	return name, epoch, nil
}

func parseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("pki: not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil { // the asker holds the key
		return nil, fmt.Errorf("pki: the request's signature does not check: %w", err)
	}
	k, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("pki: the key must be ECDSA P-256")
	}
	return csr, nil
}

func (a *Authority) sign(tmpl *x509.Certificate, csr *x509.CertificateRequest) ([]byte, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// SignClient signs a client certificate for name at epoch.
func (a *Authority) SignClient(csrPEM []byte, name string, epoch int, now time.Time) ([]byte, error) {
	if !NameRe.MatchString(name) || epoch < 1 {
		return nil, errors.New("pki: bad client name or epoch")
	}
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	return a.sign(&x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: name}, // display only: ClientOf reads the URI
		NotBefore: now.Add(-skew), NotAfter: now.Add(ClientValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{ClientURI(name, epoch)},
	}, csr)
}

// SignServing signs a serving certificate for an agent's endpoint, for the
// given host names and addresses.
func (a *Authority) SignServing(csrPEM []byte, names []string, now time.Time) ([]byte, error) {
	if len(names) == 0 || len(names) > 16 {
		return nil, errors.New("pki: 1 to 16 names")
	}
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "vigil agent endpoint"},
		NotBefore: now.Add(-skew), NotAfter: now.Add(ClientValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if hostNameRe.MatchString(n) {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		} else {
			return nil, fmt.Errorf("pki: %q is not a host name or address", n)
		}
	}
	return a.sign(tmpl, csr)
}

// NewKeyCSR makes a P-256 key and a request for it, both PEM: what a client
// (or the agent for it) sends to be signed.
func NewKeyCSR() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, nil, err
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// SerialAndExpiry reads a PEM certificate's serial (hex) and NotAfter.
func SerialAndExpiry(certPEM []byte) (string, time.Time) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return "", time.Time{}
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", time.Time{}
	}
	return c.SerialNumber.Text(16), c.NotAfter
}
