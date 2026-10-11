package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/server"
)

// What a penetration test of the web side found, kept from coming back.
func TestHardening(t *testing.T) {
	cfg := &config.Server{DataDir: t.TempDir(), IngestToken: "ingest", AdminToken: "admin-token-secret",
		HeartbeatTimeout: config.Duration(time.Minute), ExternalURL: "https://vigil.example",
		TrustedProxies: []string{"127.0.0.1"}} // the test client stands in for the reverse proxy
	cfg.Storage.MetricsURL, cfg.Storage.LogsURL = "http://127.0.0.1:1", "http://127.0.0.1:1"
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	central := httptest.NewServer(srv.Handler())
	defer central.Close()

	type opts struct {
		from, token, body string
		noCSRF            bool
		jar               http.CookieJar
	}
	call := func(method, path string, o opts) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, central.URL+path, strings.NewReader(o.body))
		if !o.noCSRF {
			req.Header.Set("X-Vigil-CSRF", "1")
		}
		if o.token != "" {
			req.Header.Set("Authorization", "Bearer "+o.token)
		}
		if o.from != "" {
			req.Header.Set("X-Forwarded-For", o.from)
		}
		hc := &http.Client{Jar: o.jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	admin := opts{token: "admin-token-secret"}
	user := func(name, pass, role string) {
		t.Helper()
		o := admin
		o.body = fmt.Sprintf(`{"password":%q,"role":%q}`, pass, role)
		if code, body, _ := call("PUT", "/api/v1/users/"+name, o); code != 204 {
			t.Fatalf("create %s: %d %s", name, code, body)
		}
	}
	signIn := func(name, pass, from string) (int, http.CookieJar) {
		jar, _ := cookiejar.New(nil)
		code, _, _ := call("POST", "/api/v1/login", opts{from: from, jar: jar, body: fmt.Sprintf(`{"username":%q,"password":%q}`, name, pass)})
		return code, jar
	}
	user("root", "correct-horse-battery", "admin")
	user("vera", "viewer-password-1", "viewer")

	// Every answer carries the headers that make a browser careful.
	for _, path := range []string{"/", "/api/v1/me", "/docs/", "/healthz", "/nothing-here"} {
		_, _, h := call("GET", path, opts{})
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Strict-Transport-Security"), "max-age=") {
			t.Errorf("%s: headers %v", path, h)
		}
	}
	if _, _, h := call("GET", "/api/v1/me", opts{}); h.Get("Cache-Control") != "no-store" {
		t.Errorf("API answers must not be cached: %q", h.Get("Cache-Control"))
	}
	// The files of the interface: no listings, no methods but reading.
	for _, path := range []string{"/js/", "/vendor/"} {
		if code, body, _ := call("GET", path, opts{}); code != 404 || strings.Contains(body, "main.js") {
			t.Errorf("directory listing at %s: %d", path, code)
		}
	}
	for _, m := range []string{"TRACE", "PATCH", "POST", "DELETE"} {
		if code, _, _ := call(m, "/", opts{}); code != http.StatusMethodNotAllowed {
			t.Errorf("%s /: %d", m, code)
		}
	}

	// A sign-in must say it comes from the interface: a form on another
	// site cannot sign the visitor in to an account of its choosing.
	if code, _, _ := call("POST", "/api/v1/login", opts{noCSRF: true, body: `{"username":"vera","password":"viewer-password-1"}`}); code != 403 {
		t.Errorf("sign-in without the header: %d", code)
	}
	if code, _ := signIn("vera", "viewer-password-1", "198.51.100.1"); code != 204 {
		t.Fatalf("sign-in: %d", code)
	}
	// Behind a trusted proxy of an https site, the cookie is Secure and not readable by scripts.
	_, _, h := call("POST", "/api/v1/login", opts{from: "198.51.100.1", body: `{"username":"vera","password":"viewer-password-1"}`})
	if c := h.Get("Set-Cookie"); !strings.Contains(c, "Secure") || !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
		t.Errorf("session cookie: %s", c)
	}

	// Guessing is limited per real client address, read from the trusted
	// proxy's header: one address is locked out, another is not.
	for i := 0; i < 10; i++ {
		signIn("vera", "wrong", "203.0.113.5")
	}
	if code, _ := signIn("vera", "viewer-password-1", "203.0.113.5"); code != http.StatusTooManyRequests {
		t.Errorf("the guessing address should be locked out: %d", code)
	}
	if code, _ := signIn("vera", "viewer-password-1", "198.51.100.1"); code != 204 {
		t.Errorf("another address must still get in: %d", code)
	}
	// A client cannot hide behind a header it wrote itself: only the hop
	// the proxy appended counts.
	if code, _ := signIn("vera", "viewer-password-1", "9.9.9.9, 203.0.113.5"); code != http.StatusTooManyRequests {
		t.Errorf("a forged first hop got around the limit: %d", code)
	}
	// Many addresses guessing one account are stopped too.
	for i := 0; i < 20; i++ {
		signIn("root", "wrong", fmt.Sprintf("192.0.2.%d", i+1))
	}
	if code, _ := signIn("root", "correct-horse-battery", "192.0.2.200"); code != http.StatusTooManyRequests {
		t.Errorf("an account under attack from many addresses should be locked: %d", code)
	}
	// An unknown name is answered like a wrong password.
	if code, body, _ := call("POST", "/api/v1/login", opts{from: "192.0.2.250", body: `{"username":"nobody","password":"x"}`}); code != 401 || !strings.Contains(body, "wrong user name or password") {
		t.Errorf("unknown user: %d %s", code, body)
	}

	// Sessions end when the password or the role changes.
	user("sam", "sams-first-password", "viewer")
	_, phone := signIn("sam", "sams-first-password", "198.51.100.2")
	_, laptop := signIn("sam", "sams-first-password", "198.51.100.3")
	// Tests talk plain HTTP, so hand the Secure cookie over by hand.
	cookie := func(j http.CookieJar) string {
		u := mustURL(strings.Replace(central.URL, "http://", "https://", 1))
		for _, c := range j.Cookies(u) {
			return c.Name + "=" + c.Value
		}
		return ""
	}
	as := func(c, method, path, body string) int {
		req, _ := http.NewRequest(method, central.URL+path, strings.NewReader(body))
		req.Header.Set("Cookie", c)
		req.Header.Set("X-Vigil-CSRF", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	p, l := cookie(phone), cookie(laptop)
	if p == "" || as(p, "GET", "/api/v1/me", "") != 200 || as(l, "GET", "/api/v1/me", "") != 200 {
		t.Fatalf("two sessions should be live")
	}
	if code := as(l, "POST", "/api/v1/me/password", `{"old":"sams-first-password","new":"sams-second-password"}`); code != 204 {
		t.Fatalf("change own password: %d", code)
	}
	if as(l, "GET", "/api/v1/me", "") != 200 || as(p, "GET", "/api/v1/me", "") != 401 {
		t.Errorf("after changing the password the other session must be gone, this one kept")
	}
	user("sam", "sams-third-password", "viewer") // an admin resets it
	if as(l, "GET", "/api/v1/me", "") != 401 {
		t.Errorf("a password reset by an admin must end the user's sessions")
	}
	_, again := signIn("sam", "sams-third-password", "198.51.100.2")
	a := cookie(again)
	o := admin
	o.body = `{"role":"admin"}`
	call("PUT", "/api/v1/users/sam", o) // promoted without a new password
	if as(a, "GET", "/api/v1/me", "") != 401 {
		t.Errorf("a change of role must end the user's sessions")
	}

	// A viewer reads profiles, but not the credentials in them.
	o = admin
	o.body = "config:\n  scrape:\n    static:\n      - job: db\n        targets: [\"localhost:9187\"]\n        bearer_token: s3cr3t-token-value\n" +
		"      - {job: app, targets: [\"localhost:1\"], basic_auth: {username: mon, password: \"hunter2 with spaces\"}}\n"
	if code, body, _ := call("PUT", "/api/v1/fleet/profiles/secrets", o); code != 200 {
		t.Fatalf("profile: %d %s", code, body)
	}
	_, vjar := signIn("vera", "viewer-password-1", "198.51.100.9")
	v := cookie(vjar)
	read := func(c, token, path string) string {
		req, _ := http.NewRequest("GET", central.URL+path, nil)
		if c != "" {
			req.Header.Set("Cookie", c)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, path := range []string{"/api/v1/fleet/profiles/secrets", "/api/v1/fleet/profiles/secrets/versions/1", "/api/v1/agents/any-host/config"} {
		seen := read(v, "", path)
		if strings.Contains(seen, "s3cr3t") || strings.Contains(seen, "hunter2") || !strings.Contains(seen, "(hidden)") || !strings.Contains(seen, "mon") {
			t.Errorf("viewer at %s sees: %s", path, seen)
		}
		if full := read("", "admin-token-secret", path); !strings.Contains(full, "s3cr3t-token-value") || !strings.Contains(full, "hunter2 with spaces") {
			t.Errorf("admin at %s should see everything: %s", path, full)
		}
	}

	// A label name is a label name: nothing else reaches the metric store's address.
	for _, name := range []string{"..%2f..%2fadmin%2ftsdb%2fdelete_series", "a%2fb", "..", "x%00"} {
		if code := as(v, "GET", "/api/v1/label/"+name+"/values", ""); code != 400 && code != 404 {
			t.Errorf("label name %q: %d", name, code)
		}
	}
	var me struct{ User, Role string }
	json.Unmarshal([]byte(read(v, "", "/api/v1/me")), &me)
	if me.Role != "viewer" {
		t.Errorf("me: %+v", me)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
