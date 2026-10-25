package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBao answers the handful of OpenBao endpoints vigil uses.
type fakeBao struct {
	logins, renewals int
	down             bool
	ttl              int
	kv1              bool
	secrets          map[string]map[string]any
}

func (f *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"errors": []string{"Vault is sealed"}})
		return
	}
	body, _ := io.ReadAll(r.Body)
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	auth := map[string]any{"client_token": "s.tok", "policies": []string{"default", "vigil"}, "lease_duration": f.ttl, "renewable": true}
	switch p := strings.TrimPrefix(r.URL.Path, "/v1/"); {
	case p == "auth/approle/login":
		var in map[string]string
		json.Unmarshal(body, &in)
		if in["role_id"] != "role-1234" || in["secret_id"] != "secret-5678" {
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]any{"errors": []string{"invalid role or secret ID"}})
			return
		}
		f.logins++
		reply(map[string]any{"auth": auth})
	case r.Header.Get("X-Vault-Token") != "s.tok":
		w.WriteHeader(http.StatusForbidden)
		reply(map[string]any{"errors": []string{"permission denied"}})
	case p == "auth/token/renew-self":
		f.renewals++
		reply(map[string]any{"auth": auth})
	case p == "auth/token/lookup-self":
		reply(map[string]any{"data": map[string]any{"ttl": f.ttl, "renewable": true, "policies": []string{"default", "vigil"}}})
	case p == "sys/internal/ui/mounts/vigil":
		v := "2"
		if f.kv1 {
			v = "1"
		}
		reply(map[string]any{"data": map[string]any{"options": map[string]string{"version": v}}})
	case p == "sys/capabilities-self":
		reply(map[string]any{"vigil/metadata/": []string{"list"}, "vigil/data/anthropic": []string{"read"}, "vigil/data/vigil-capability-probe": []string{"read"}})
	case r.Method == "LIST" && p == "vigil/metadata/":
		reply(map[string]any{"data": map[string]any{"keys": []string{"telegram", "anthropic"}}})
	case strings.HasPrefix(p, "vigil/data/") && !f.kv1:
		s, ok := f.secrets[strings.TrimPrefix(p, "vigil/data/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			reply(map[string]any{"errors": []string{}})
			return
		}
		reply(map[string]any{"data": map[string]any{"data": s, "metadata": map[string]any{"version": 3}}})
	case strings.HasPrefix(p, "vigil/") && f.kv1:
		s, ok := f.secrets[strings.TrimPrefix(p, "vigil/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(map[string]any{"data": s})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFake(t *testing.T) (*fakeBao, *OpenBao, *time.Time) {
	f := &fakeBao{ttl: 3600, secrets: map[string]map[string]any{"anthropic": {"api_key": "sk-ant-not-real-0000-abcd"}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o, err := New(Settings{Kind: "openbao", Address: srv.URL, Auth: "approle", RoleID: "role-1234", Mount: "vigil"}, "secret-5678")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	o.now = func() time.Time { return now }
	return f, o, &now
}

func TestGetRenewAndLastGood(t *testing.T) {
	f, o, now := newFake(t)
	ctx := context.Background()
	ref := Ref{Path: "anthropic", Field: "api_key"}
	if v, err := o.Get(ctx, ref); err != nil || v != "sk-ant-not-real-0000-abcd" || f.logins != 1 {
		t.Fatalf("get: %q %v logins=%d", v, err, f.logins)
	}
	// Two thirds into the token's life it is renewed, not replaced.
	*now = now.Add(41 * time.Minute)
	o.Refresh(ctx)
	if f.renewals != 1 || f.logins != 1 || !o.Health().Up {
		t.Errorf("renew: renewals=%d logins=%d %+v", f.renewals, f.logins, o.Health())
	}
	// The store goes down: the last value read is still given, and the
	// store is reported down.
	f.down = true
	*now = now.Add(10 * time.Minute)
	if v, err := o.Get(ctx, ref); err != nil || v != "sk-ant-not-real-0000-abcd" {
		t.Errorf("last good value: %q %v", v, err)
	}
	if h := o.Health(); h.Up || !strings.Contains(h.Error, "sealed") {
		t.Errorf("health while down: %+v", h)
	}
	if _, err := o.Get(ctx, Ref{Path: "anthropic", Field: "other"}); err == nil {
		t.Error("a value never read cannot come from a store that is down")
	}
	f.down = false
	if _, err := o.Get(ctx, Ref{Path: "anthropic", Field: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing field: %v", err)
	}
	if _, err := o.Get(ctx, Ref{Path: "nothing", Field: "x"}); !errors.Is(err, ErrNotFound) || !o.Health().Up {
		t.Errorf("missing secret: %v %+v", err, o.Health())
	}
	if st := o.Describe(ctx, ref); !st.Found || st.Version != 3 || st.Hint != "…abcd" {
		t.Errorf("describe: %+v", st)
	}
}

func TestTestAndList(t *testing.T) {
	_, o, _ := newFake(t)
	ctx := context.Background()
	h, caps, err := o.Test(ctx, []string{"anthropic"})
	if err != nil || !h.Up || h.KVVersion != 2 || !caps.Read["anthropic"] || !caps.List || caps.Write {
		t.Fatalf("test: %v %+v %+v", err, h, caps)
	}
	if keys, err := o.List(ctx, ""); err != nil || strings.Join(keys, ",") != "anthropic,telegram" {
		t.Errorf("list: %v %v", keys, err)
	}
	bad, _ := New(Settings{Kind: "openbao", Address: o.set.Address, Auth: "approle", RoleID: "role-1234", Mount: "vigil"}, "wrong-secret")
	if _, _, err := bad.Test(ctx, nil); err == nil || !strings.Contains(err.Error(), "invalid role or secret ID") {
		t.Errorf("a wrong secret_id: %v", err)
	}
}

func TestKV1(t *testing.T) {
	f, o, _ := newFake(t)
	f.kv1 = true
	if v, err := o.Get(context.Background(), Ref{Path: "anthropic", Field: "api_key"}); err != nil || v != "sk-ant-not-real-0000-abcd" {
		t.Errorf("kv1: %q %v", v, err)
	}
}

func TestSettingsCheck(t *testing.T) {
	for _, s := range []Settings{
		{Kind: "openbao", Address: "ftp://x", Auth: "token", Mount: "vigil"},
		{Kind: "openbao", Address: "http://x:8200/v1", Auth: "token", Mount: "vigil"},
		{Kind: "openbao", Address: "http://x:8200", Auth: "approle", Mount: "vigil"}, // no role_id
		{Kind: "openbao", Address: "http://x:8200", Auth: "token", Mount: "../sys"},
		{Kind: "vault9", Address: "http://x:8200", Auth: "token", Mount: "vigil"},
	} {
		if err := s.Check(); err == nil {
			t.Errorf("accepted: %+v", s)
		}
	}
	ok := Settings{Kind: "OpenBao", Address: "https://bao.example.com:8200/", Auth: "approle", RoleID: "role-1234", Mount: "/vigil/"}
	if err := ok.Check(); err != nil || ok.Address != "https://bao.example.com:8200" || ok.Mount != "vigil" || ok.AuthMount != "approle" {
		t.Errorf("normalised: %v %+v", err, ok)
	}
	r := Ref{Path: " /anthropic/ ", Field: "api_key"}
	if r.Check() != nil || r.Path != "anthropic" {
		t.Errorf("ref: %+v", r)
	}
}

func TestSealer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.key")
	s, err := OpenSealer(path)
	if err != nil {
		t.Fatal(err)
	}
	box := s.Seal("store", "secret-5678")
	if strings.Contains(box, "secret-5678") {
		t.Fatal("not encrypted")
	}
	again, _ := OpenSealer(path) // the same key file
	if v, err := again.Open("store", box); err != nil || v != "secret-5678" {
		t.Errorf("open: %q %v", v, err)
	}
	if _, err := again.Open("elsewhere", box); err == nil {
		t.Error("opened under another label")
	}
	other, _ := OpenSealer(filepath.Join(t.TempDir(), "other.key"))
	if _, err := other.Open("store", box); err == nil {
		t.Error("opened with another key file")
	}
}
