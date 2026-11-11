package opscli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIdentityCommand(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(b))
		mu.Unlock()
		switch {
		case r.Method == "GET":
			w.Write([]byte(`[{"name":"portun-chat","kind":"client","agents":"host-1","epoch":3,"revoked":false,"last_issued":"2026-11-02T07:48:52Z"},
				{"name":"otlp-secure","kind":"endpoint","agents":"gw","names":["192.0.2.10","otlp.internal"],"epoch":1,"revoked":true}]`))
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/nope"):
			http.Error(w, "not allowed", http.StatusForbidden)
		default:
			w.Write([]byte(`{"name":"x","kind":"client","agents":"a","epoch":2,"revoked":false}`))
		}
	}))
	defer srv.Close()
	conn := []string{"-server", srv.URL, "-token", "admin-test"}
	run := func(args ...string) error { return Identity(append(args, conn...)) }

	if err := run("approve", "portun-chat", "-agents", "host-1"); err != nil {
		t.Fatal(err)
	}
	if err := Identity(append([]string{"approve", "-agents", "gw", "-endpoint", "-names", "192.0.2.10, otlp.internal"}, append(conn, "otlp-secure")...)); err != nil {
		t.Fatal(err)
	}
	if err := run("revoke", "portun-chat"); err != nil {
		t.Fatal(err)
	}
	if err := run("delete", "portun-chat"); err != nil {
		t.Fatal(err)
	}
	if err := run("revoke", "nope"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("a refusal is not an error: %v", err)
	}
	want := []string{
		`PUT /api/v1/identities/portun-chat Bearer admin-test {"agents":"host-1","kind":"client"}`,
		`PUT /api/v1/identities/otlp-secure Bearer admin-test {"agents":"gw","kind":"endpoint","names":["192.0.2.10","otlp.internal"]}`,
		`POST /api/v1/identities/portun-chat/revoke Bearer admin-test `,
		`DELETE /api/v1/identities/portun-chat Bearer admin-test `,
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("request %d:\n got %s\nwant %s", i, got[i], w)
		}
	}
	for _, bad := range [][]string{{"approve", "x"}, {"approve", "x", "-agents", "a", "-names", "h"}, {"revoke"}, {"frobnicate", "x"}} {
		if err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}

	var out bytes.Buffer
	body := `[{"name":"portun-chat","kind":"client","agents":"host-1","epoch":3,"revoked":false},{"name":"gw","kind":"endpoint","agents":"g","names":["192.0.2.10"],"epoch":1,"revoked":true}]`
	if err := printIdentities(&out, body); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "portun-chat") || !strings.Contains(s, "approved") || !strings.Contains(s, "revoked") || !strings.Contains(s, "192.0.2.10") || !strings.Contains(s, "never") {
		t.Errorf("table:\n%s", s)
	}
}
