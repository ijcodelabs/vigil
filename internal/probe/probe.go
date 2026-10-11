// Package probe checks from the agent's host that an address answers.
package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

const defaultTimeout = 10 * time.Second

// Check runs one probe and returns its result as metrics, labelled with the
// probe's name:
//
//	probe_success                   1 if it answered as expected
//	probe_duration_seconds          how long the answer took
//	probe_http_status_code          (HTTP) the status, 0 if there was none
//	probe_tls_cert_expiry_seconds   (HTTPS) time left on the certificate that expires first
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
		l := map[string]string{"probe": p.Name}
		for k, val := range p.Labels {
			l[k] = val
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}
	ok := false
	if p.TCP != "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.TCP)
		if err == nil {
			conn.Close()
			ok = true
		}
	} else {
		ok = checkHTTP(ctx, p, add)
	}
	add("probe_duration_seconds", time.Since(start).Seconds())
	success := 0.0
	if ok {
		success = 1
	}
	add("probe_success", success)
	return out
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
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: p.TLSInsecure},
	}}
	resp, err := client.Do(req)
	if err != nil {
		add("probe_http_status_code", 0)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	add("probe_http_status_code", float64(resp.StatusCode))
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		earliest := resp.TLS.PeerCertificates[0].NotAfter
		for _, c := range resp.TLS.PeerCertificates[1:] {
			if c.NotAfter.Before(earliest) {
				earliest = c.NotAfter
			}
		}
		add("probe_tls_cert_expiry_seconds", time.Until(earliest).Seconds())
	}
	if len(p.ExpectStatus) > 0 {
		return slices.Contains(p.ExpectStatus, resp.StatusCode)
	}
	return resp.StatusCode/100 == 2
}
