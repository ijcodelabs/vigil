package otlp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ijcodelabs/vigil/internal/model"
)

const (
	gatewayToken = "gateway-test-token-xxxx"
	boopToken    = "boop-test-token-xxxxxxx"
)

func writeTokens(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "otlp-tokens")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func withToken(t *testing.T, h http.Handler, path, token string, m proto.Message) int {
	t.Helper()
	b, _ := proto.Marshal(m)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/x-protobuf")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func boopEvent(attrs ...*commonpb.KeyValue) *collogs.ExportLogsServiceRequest {
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{Resource: resource, ScopeLogs: []*logspb.ScopeLogs{{
		LogRecords: []*logspb.LogRecord{{TimeUnixNano: 1_700_000_000_000_000_000,
			Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boop.session.started"}},
			Attributes: attrs}}}}}}}
}

func TestTokens(t *testing.T) {
	var logs []model.LogEntry
	var samples []model.Sample
	r := &Receiver{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Logs:    func(e []model.LogEntry) error { logs = append(logs, e...); return nil },
		Metrics: func(s []model.Sample) { samples = append(samples, s...) }}
	path := writeTokens(t, "# emitters\nboop-jump-gateway "+gatewayToken+"\n\n")
	tokens, err := newTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := requireToken(tokens, r.Handler())

	for _, tok := range []string{"", "wrong-test-token-xxxxx", gatewayToken + "x"} {
		if c := withToken(t, h, "/v1/logs", tok, boopEvent()); c != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, c)
		}
	}
	if len(logs) != 0 {
		t.Fatalf("refused requests delivered %d entries", len(logs))
	}

	// The name comes from the token; a sender's own "emitter" is kept apart.
	if c := withToken(t, h, "/v1/logs", gatewayToken, boopEvent(str("emitter", "boop"), str("agent", "agt-1"))); c != 200 {
		t.Fatalf("good token: %d", c)
	}
	if e := logs[0]; e.Stream["emitter"] != "boop-jump-gateway" || e.Fields["attr_emitter"] != "boop" || e.Fields["agent"] != "agt-1" {
		t.Errorf("entry: %+v", e)
	}
	metrics := &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: resource,
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "queue_depth",
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{dbl(3, str("emitter", "boop"))}}}}}}}}}}
	if c := withToken(t, h, "/v1/metrics", gatewayToken, metrics); c != 200 {
		t.Fatalf("metrics: %d", c)
	}
	if l := samples[0].Labels; l["emitter"] != "boop-jump-gateway" || l["exported_emitter"] != "boop" {
		t.Errorf("labels: %v", l)
	}

	// A token added to the file is taken without a restart; a removed one
	// is refused.
	os.WriteFile(path, []byte("boop "+boopToken+"\n"), 0o600)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	tokens.mu.Lock()
	tokens.checked = time.Time{}
	tokens.mu.Unlock()
	if c := withToken(t, h, "/v1/logs", boopToken, boopEvent()); c != 200 {
		t.Errorf("new token: %d", c)
	}
	if c := withToken(t, h, "/v1/logs", gatewayToken, boopEvent()); c != http.StatusUnauthorized {
		t.Errorf("removed token: %d", c)
	}

	// A broken file keeps the tokens from before.
	os.WriteFile(path, []byte("only-a-name\n"), 0o600)
	future = future.Add(time.Minute)
	os.Chtimes(path, future, future)
	tokens.mu.Lock()
	tokens.checked = time.Time{}
	tokens.mu.Unlock()
	if c := withToken(t, h, "/v1/logs", boopToken, boopEvent()); c != 200 {
		t.Errorf("after a broken file: %d", c)
	}
}

func TestTokenFileErrors(t *testing.T) {
	for _, body := range []string{
		"boop short",
		"Boop " + boopToken,
		"boop " + boopToken + " extra",
		"boop " + boopToken + "\ngateway " + boopToken,
	} {
		if _, err := parseTokens([]byte(body)); err == nil {
			t.Errorf("%q: accepted", body)
		}
	}
	// One name, two tokens: a rotation.
	got, err := parseTokens([]byte("boop " + boopToken + "\nboop " + gatewayToken))
	if err != nil || len(got) != 2 {
		t.Errorf("rotation: %v %v", got, err)
	}
	// A missing file: started, but nobody gets in.
	tf, err := newTokenFile(filepath.Join(t.TempDir(), "none"))
	if err == nil {
		t.Error("missing file: no error")
	}
	h := requireToken(tf, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	if c := withToken(t, h, "/v1/logs", boopToken, boopEvent()); c != http.StatusUnauthorized {
		t.Errorf("missing file let a request in: %d", c)
	}
}

func TestTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool := selfSigned(t, dir)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	got := make(chan model.LogEntry, 1)
	r := &Receiver{Listen: addr, CertFile: certFile, KeyFile: keyFile,
		TokensFile: writeTokens(t, "boop "+boopToken+"\n"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Logs:       func(e []model.LogEntry) error { got <- e[0]; return nil },
		Metrics:    func([]model.Sample) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	b, _ := proto.Marshal(boopEvent())
	var resp *http.Response
	var err error
	for range 50 {
		req, _ := http.NewRequest("POST", "https://"+addr+"/v1/logs", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+boopToken)
		if resp, err = client.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e := <-got; e.Stream["emitter"] != "boop" {
		t.Errorf("entry: %+v", e)
	}
	// Plain HTTP is not answered as OTLP.
	plain, err := http.Post("http://"+addr+"/v1/logs", "application/x-protobuf", bytes.NewReader(b))
	if err == nil {
		body, _ := io.ReadAll(plain.Body)
		plain.Body.Close()
		if plain.StatusCode == 200 || strings.Contains(string(body), "partial") {
			t.Errorf("plain HTTP: %d", plain.StatusCode)
		}
	}
}

func selfSigned(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "otlp test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}
