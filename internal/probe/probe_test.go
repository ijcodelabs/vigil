package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

func values(p config.Probe) map[string]float64 {
	out := map[string]float64{}
	for _, s := range Check(context.Background(), p) {
		out[s.Name] = s.Value
		if s.Labels["probe"] != p.Name {
			out["wrong-label"] = 1
		}
	}
	return out
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ok := values(config.Probe{Name: "shop", HTTP: srv.URL, TLSInsecure: true, Labels: map[string]string{"team": "web"}})
	if ok["probe_success"] != 1 || ok["probe_http_status_code"] != 200 || ok["probe_tls_cert_expiry_seconds"] <= 0 || ok["wrong-label"] != 0 {
		t.Errorf("healthy: %v", ok)
	}
	// The test server's certificate is not trusted: without the opt-out it is a failure.
	if v := values(config.Probe{Name: "shop", HTTP: srv.URL}); v["probe_success"] != 0 || v["probe_http_status_code"] != 0 {
		t.Errorf("untrusted certificate: %v", v)
	}
	if v := values(config.Probe{Name: "shop", HTTP: srv.URL + "/missing", TLSInsecure: true}); v["probe_success"] != 0 || v["probe_http_status_code"] != 404 {
		t.Errorf("404: %v", v)
	}
	if v := values(config.Probe{Name: "shop", HTTP: srv.URL + "/missing", TLSInsecure: true, ExpectStatus: []int{404}}); v["probe_success"] != 1 {
		t.Errorf("expected 404: %v", v)
	}
}

func TestTCPAndTimeout(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	if v := values(config.Probe{Name: "db", TCP: ln.Addr().String()}); v["probe_success"] != 1 {
		t.Errorf("open port: %v", v)
	}
	if v := values(config.Probe{Name: "db", TCP: "127.0.0.1:1"}); v["probe_success"] != 0 {
		t.Errorf("closed port: %v", v)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	start := time.Now()
	v := values(config.Probe{Name: "slow", HTTP: slow.URL, Timeout: config.Duration(200 * time.Millisecond)})
	if v["probe_success"] != 0 || time.Since(start) > time.Second {
		t.Errorf("timeout: %v after %v", v, time.Since(start))
	}
}
