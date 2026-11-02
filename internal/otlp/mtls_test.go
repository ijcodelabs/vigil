package otlp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/pki"
	"google.golang.org/protobuf/proto"
)

// TestClientCerts: a certificate from the authority at the current epoch is
// the emitter; an older epoch, an unknown name, a token naming someone else
// and (when required) no certificate are refused.
func TestClientCerts(t *testing.T) {
	ca, err := pki.Open(t.TempDir(), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	issue := func(name string, epoch int) tls.Certificate {
		key, csr, _ := pki.NewKeyCSR()
		var c []byte
		var err error
		if name == "" {
			c, err = ca.SignServing(csr, []string{"127.0.0.1"}, now)
		} else {
			c, err = ca.SignClient(csr, name, epoch, now)
		}
		if err != nil {
			t.Fatal(err)
		}
		kp, err := tls.X509KeyPair(c, key)
		if err != nil {
			t.Fatal(err)
		}
		return kp
	}
	serving := issue("", 0)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CAPEM())

	var mu sync.Mutex
	epochs := map[string]int{"boop-chat": 2, "boop": 1}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	got := make(chan model.LogEntry, 4)
	r := &Receiver{Listen: addr, ClientCerts: "required",
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serving, nil },
		ClientCAs:      func() *x509.CertPool { return pool },
		Epochs:         func() map[string]int { mu.Lock(); defer mu.Unlock(); return epochs },
		TokensFile:     writeTokens(t, "boop "+boopToken+"\n"),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Logs:           func(e []model.LogEntry) error { got <- e[0]; return nil },
		Metrics:        func([]model.Sample) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	b, _ := proto.Marshal(boopEvent())
	post := func(cert *tls.Certificate, token string) (int, error) {
		cfg := &tls.Config{RootCAs: pool}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		var err error
		for range 50 {
			req, _ := http.NewRequest("POST", "https://"+addr+"/v1/logs", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/x-protobuf")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			var resp *http.Response
			if resp, err = client.Do(req); err == nil {
				resp.Body.Close()
				return resp.StatusCode, nil
			}
			if cert == nil { // a refused handshake is the answer, not a slow start
				return 0, err
			}
			time.Sleep(20 * time.Millisecond)
		}
		return 0, err
	}

	chat := issue("boop-chat", 2)
	if c, err := post(&chat, ""); err != nil || c != 200 {
		t.Fatalf("current certificate: %d %v", c, err)
	}
	if e := <-got; e.Stream["emitter"] != "boop-chat" {
		t.Errorf("emitter: %+v", e.Stream)
	}
	old := issue("boop-chat", 1)
	if c, _ := post(&old, ""); c != http.StatusForbidden {
		t.Errorf("older epoch: %d", c)
	}
	stranger := issue("stranger", 1)
	if c, _ := post(&stranger, ""); c != http.StatusForbidden {
		t.Errorf("unknown name: %d", c)
	}
	if c, _ := post(&chat, boopToken); c != http.StatusForbidden {
		t.Errorf("token for another emitter: %d", c)
	}
	boop := issue("boop", 1)
	if c, _ := post(&boop, boopToken); c != 200 {
		t.Errorf("token and certificate agree: %d", c)
	}
	<-got
	if c, err := post(nil, boopToken); err == nil {
		t.Errorf("no certificate with required: %d", c)
	}

	// Revoked while connected: the next request on the same connection stops.
	cfg := &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{chat}}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	send := func() int {
		req, _ := http.NewRequest("POST", "https://"+addr+"/v1/logs", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := send(); c != 200 {
		t.Fatalf("before revoking: %d", c)
	}
	<-got
	mu.Lock()
	epochs = map[string]int{"boop-chat": 3, "boop": 1}
	mu.Unlock()
	if c := send(); c != http.StatusForbidden {
		t.Errorf("after revoking, same connection: %d", c)
	}
}
