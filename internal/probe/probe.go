// Package probe checks from the agent's host that an address answers.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

const (
	defaultTimeout = 10 * time.Second
	maxBody        = 1 << 20 // how much of an answer expect_body looks at
)

// Check runs one probe and returns its result as metrics, labelled with the
// probe's name, its type (http, tcp, tls, ping) and what it checks:
//
//	probe_success                   1 if it answered as expected
//	probe_duration_seconds          how long the answer took (ping: the average round trip)
//	probe_http_status_code          (HTTP) the status, 0 if there was none
//	probe_tls_cert_expiry_seconds   (HTTPS, TLS) time left on the certificate that expires
//	                                first; negative once it has expired
//	probe_packet_loss_ratio         (ping) share of the echo requests that got no answer
func Check(ctx context.Context, p config.Probe) []model.Sample {
	timeout := p.Timeout.Std()
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ts := start.UnixMilli()
	var out []model.Sample
	add := func(name string, v float64) {
		l := map[string]string{"probe": p.Name, "type": p.Kind(), "target": target(p)}
		for k, val := range p.Labels {
			l[k] = val
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}
	ok := false
	took := time.Duration(-1)
	switch p.Kind() {
	case "tcp":
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.TCP)
		if err == nil {
			conn.Close()
			ok = true
		}
	case "tls":
		ok = checkTLS(ctx, p, add)
	case "ping":
		ok, took = checkPing(ctx, p.Ping, add)
	default:
		ok = checkHTTP(ctx, p, add)
	}
	if took < 0 {
		took = time.Since(start)
	}
	add("probe_duration_seconds", took.Seconds())
	success := 0.0
	if ok {
		success = 1
	}
	add("probe_success", success)
	return out
}

// target is what the probe checks, as a label: without the user name,
// password and query a URL may carry, which are often secrets.
func target(p config.Probe) string {
	if p.HTTP == "" {
		return p.Target()
	}
	u, err := url.Parse(p.HTTP)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// tlsConfig checks the certificate itself instead of leaving it to the
// handshake, so that the time left is reported also for a certificate that
// is refused: one that has expired is exactly the one people ask about.
//
// Only the first connection is measured: that is the address the probe
// names. Where it redirects to has a certificate of its own, and reporting
// that one instead hides the day the named address stops working.
func tlsConfig(p config.Probe, add func(string, float64)) *tls.Config {
	var first sync.Once
	return &tls.Config{
		InsecureSkipVerify: true, // verified below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			earliest := cs.PeerCertificates[0].NotAfter
			for _, c := range cs.PeerCertificates[1:] {
				if c.NotAfter.Before(earliest) {
					earliest = c.NotAfter
				}
			}
			first.Do(func() { add("probe_tls_cert_expiry_seconds", time.Until(earliest).Seconds()) })
			if p.TLSInsecure {
				return nil
			}
			opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: x509.NewCertPool()}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		},
	}
}

func checkTLS(ctx context.Context, p config.Probe, add func(string, float64)) bool {
	host, _, _ := net.SplitHostPort(p.TLS)
	conf := tlsConfig(p, add)
	conf.ServerName = host
	conn, err := (&tls.Dialer{Config: conf}).DialContext(ctx, "tcp", p.TLS)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func checkHTTP(ctx context.Context, p config.Probe, add func(string, float64)) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.HTTP, nil)
	if err != nil {
		add("probe_http_status_code", 0)
		return false
	}
	req.Header.Set("User-Agent", "vigil-probe")
	// A fresh connection every time: the point is to test that one can be made.
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   tlsConfig(p, add),
	}}
	resp, err := client.Do(req)
	if err != nil {
		add("probe_http_status_code", 0)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	add("probe_http_status_code", float64(resp.StatusCode))
	if len(p.ExpectStatus) > 0 {
		if !slices.Contains(p.ExpectStatus, resp.StatusCode) {
			return false
		}
	} else if resp.StatusCode/100 != 2 {
		return false
	}
	if p.ExpectBody != "" {
		re, err := regexp.Compile(p.ExpectBody)
		return err == nil && re.Match(body)
	}
	return true
}
