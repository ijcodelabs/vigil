// Package secrets reads the secrets vigil uses (a language model's API key,
// later the receivers' tokens) from a secret store: OpenBao, or Vault, which
// speaks the same API. vigil logs in with an AppRole or a token of its own,
// keeps that login fresh, reads KV secrets, and keeps the values in memory
// only, re-reading them every few minutes. When the store cannot be reached
// the last value read is used, so a sealed store stops nothing that already
// worked.
package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Settings say where the store is and how vigil logs in. None of it is
// secret: the secret half of the login (secret_id or token) is kept apart.
type Settings struct {
	Kind      string `json:"kind"`                // openbao
	Address   string `json:"address"`             // e.g. https://bao.example.com:8200
	CACert    string `json:"ca_cert,omitempty"`   // PEM; empty: the system's authorities
	Namespace string `json:"namespace,omitempty"` // OpenBao / Vault Enterprise namespaces
	Auth      string `json:"auth"`                // approle or token
	AuthMount string `json:"auth_mount,omitempty"`
	RoleID    string `json:"role_id,omitempty"`
	Mount     string `json:"mount"` // the KV mount, e.g. vigil
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// The login's secret half: AppRole secret ids are UUIDs, tokens are
	// opaque, neither has spaces or control characters.
	credRe = regexp.MustCompile(`^[\x21-\x7e]{8,512}$`)
)

// Check normalises the settings and says what is wrong with them.
func (s *Settings) Check() error {
	s.Kind, s.Auth = strings.ToLower(strings.TrimSpace(s.Kind)), strings.ToLower(strings.TrimSpace(s.Auth))
	s.Address, s.Mount = strings.TrimRight(strings.TrimSpace(s.Address), "/"), strings.Trim(strings.TrimSpace(s.Mount), "/")
	s.RoleID, s.Namespace = strings.TrimSpace(s.RoleID), strings.Trim(strings.TrimSpace(s.Namespace), "/")
	s.AuthMount = strings.Trim(strings.TrimSpace(s.AuthMount), "/")
	if s.Kind != "openbao" {
		return fmt.Errorf("kind %q is not known (openbao)", s.Kind)
	}
	u, err := url.Parse(s.Address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return errors.New("address must be http(s)://host:port, without a path")
	}
	switch s.Auth {
	case "approle":
		if s.RoleID == "" || !credRe.MatchString(s.RoleID) {
			return errors.New("an AppRole login needs its role_id")
		}
		if s.AuthMount == "" {
			s.AuthMount = "approle"
		}
	case "token":
		s.RoleID, s.AuthMount = "", ""
	default:
		return fmt.Errorf("auth %q is not known (approle or token)", s.Auth)
	}
	if !nameRe.MatchString(s.Mount) || (s.AuthMount != "" && !nameRe.MatchString(s.AuthMount)) || (s.Namespace != "" && !nameRe.MatchString(s.Namespace)) {
		return errors.New("mount, auth mount and namespace are names like vigil or team/vigil")
	}
	if s.CACert != "" {
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(s.CACert)) {
			return errors.New("ca_cert is not a PEM certificate")
		}
	}
	return nil
}

// CheckCredential says whether a secret_id or token looks like one.
func CheckCredential(c string) error {
	if !credRe.MatchString(c) {
		return errors.New("the secret_id or token has spaces, is too short or too long")
	}
	return nil
}

// Ref names one value: a secret's path under the mount and a field in it.
type Ref struct {
	Path  string `json:"path"`
	Field string `json:"field"`
}

func (r *Ref) Check() error {
	r.Path, r.Field = strings.Trim(strings.TrimSpace(r.Path), "/"), strings.TrimSpace(r.Field)
	if !nameRe.MatchString(r.Path) || strings.Contains(r.Path, "..") {
		return errors.New("path is a secret's name under the mount, like anthropic or ai/anthropic")
	}
	if r.Field == "" || len(r.Field) > 200 || strings.ContainsAny(r.Field, "\x00\n") {
		return errors.New("field names the key inside the secret, like api_key")
	}
	return nil
}

// Health is what the store looks like from vigil now.
type Health struct {
	Up          bool      `json:"up"`
	Checked     time.Time `json:"checked,omitzero"`
	Error       string    `json:"error,omitempty"`
	TokenExpiry time.Time `json:"token_expiry,omitzero"` // zero: does not expire
	Renewable   bool      `json:"renewable"`
	Policies    []string  `json:"policies,omitempty"`
	KVVersion   int       `json:"kv_version,omitempty"`
}

type cached struct {
	value   string
	version int
	read    time.Time
}

// OpenBao is a logged-in client of one store.
type OpenBao struct {
	set  Settings
	cred string
	http *http.Client
	now  func() time.Time
	// MaxAge is how long a value read is used before it is read again.
	MaxAge time.Duration

	mu       sync.Mutex
	token    string
	expiry   time.Time // zero: no expiry
	ttl      time.Duration
	renew    bool
	policies []string
	kv       int
	cache    map[Ref]cached
	health   Health
}

// New makes a client; it logs in on first use.
func New(set Settings, cred string) (*OpenBao, error) {
	if err := set.Check(); err != nil {
		return nil, err
	}
	if err := CheckCredential(cred); err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if set.CACert != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(set.CACert))
		tr.TLSClientConfig.RootCAs = pool
	}
	tr.Proxy = nil // the store is reached directly, never through a proxy from the environment
	return &OpenBao{set: set, cred: cred, http: &http.Client{Timeout: 15 * time.Second, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now: time.Now, MaxAge: 5 * time.Minute, cache: map[Ref]cached{}}, nil
}

// Settings returns what the client was made with.
func (o *OpenBao) Settings() Settings { return o.set }

// apiError is an answer of the store other than success; it never carries
// a secret, only the store's own error text.
type apiError struct {
	Status int
	Errors []string
}

func (e *apiError) Error() string {
	msg := strings.Join(e.Errors, "; ")
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("the store answered %d: %s", e.Status, msg)
}

func (o *OpenBao) call(ctx context.Context, method, path, token string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.set.Address+"/v1/"+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if o.set.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", o.set.Namespace)
	}
	req.Header.Set("X-Vault-Request", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("the store cannot be reached: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		e := &apiError{Status: resp.StatusCode}
		var b struct{ Errors []string }
		if json.Unmarshal(data, &b) == nil {
			e.Errors = b.Errors
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type authInfo struct {
	Auth *struct {
		ClientToken   string   `json:"client_token"`
		Policies      []string `json:"policies"`
		LeaseDuration int      `json:"lease_duration"`
		Renewable     bool     `json:"renewable"`
	} `json:"auth"`
}

// login gets a token: by AppRole, or the token given. Called with o.mu held.
func (o *OpenBao) login(ctx context.Context) error {
	now := o.now()
	switch o.set.Auth {
	case "approle":
		var a authInfo
		if err := o.call(ctx, http.MethodPost, "auth/"+o.set.AuthMount+"/login", "", map[string]string{"role_id": o.set.RoleID, "secret_id": o.cred}, &a); err != nil {
			return fmt.Errorf("AppRole login failed: %w", err)
		}
		if a.Auth == nil || a.Auth.ClientToken == "" {
			return errors.New("AppRole login gave no token")
		}
		o.token, o.renew, o.policies = a.Auth.ClientToken, a.Auth.Renewable, a.Auth.Policies
		o.ttl = time.Duration(a.Auth.LeaseDuration) * time.Second
	default:
		o.token = o.cred
		if err := o.lookup(ctx); err != nil {
			o.token = ""
			return fmt.Errorf("the token is not accepted: %w", err)
		}
		return nil
	}
	o.expiry = time.Time{}
	if o.ttl > 0 {
		o.expiry = now.Add(o.ttl)
	}
	return nil
}

// lookup reads the token's own time to live. Called with o.mu held.
func (o *OpenBao) lookup(ctx context.Context) error {
	var r struct {
		Data struct {
			TTL       int      `json:"ttl"`
			Renewable bool     `json:"renewable"`
			Policies  []string `json:"policies"`
		} `json:"data"`
	}
	if err := o.call(ctx, http.MethodGet, "auth/token/lookup-self", o.token, nil, &r); err != nil {
		return err
	}
	o.renew, o.policies = r.Data.Renewable, r.Data.Policies
	o.ttl, o.expiry = time.Duration(r.Data.TTL)*time.Second, time.Time{}
	if r.Data.TTL > 0 {
		o.expiry = o.now().Add(o.ttl)
	}
	return nil
}

// ensure makes sure there is a token with a third of its life left:
// renewed if it can be, else a new login. Called with o.mu held.
func (o *OpenBao) ensure(ctx context.Context) error {
	if o.token == "" {
		return o.login(ctx)
	}
	if o.expiry.IsZero() || o.expiry.Sub(o.now()) > o.ttl/3 {
		return nil
	}
	if o.renew {
		var a authInfo
		if err := o.call(ctx, http.MethodPost, "auth/token/renew-self", o.token, map[string]string{}, &a); err == nil && a.Auth != nil {
			o.ttl = time.Duration(a.Auth.LeaseDuration) * time.Second
			o.expiry = o.now().Add(o.ttl)
			if o.ttl > 0 && o.set.Auth == "approle" && o.ttl < 2*time.Minute {
				return o.login(ctx) // at its maximum life: start over
			}
			return nil
		}
	}
	if o.set.Auth == "approle" {
		return o.login(ctx)
	}
	return o.lookup(ctx) // a token that cannot be renewed: report how long it has left
}

// kvVersion asks the store which version the mount is; 2 when it may not
// say. Called with o.mu held.
func (o *OpenBao) kvVersion(ctx context.Context) int {
	if o.kv != 0 {
		return o.kv
	}
	var r struct {
		Data struct {
			Options map[string]string `json:"options"`
		} `json:"data"`
	}
	o.kv = 2
	if o.call(ctx, http.MethodGet, "sys/internal/ui/mounts/"+o.set.Mount, o.token, nil, &r) == nil && r.Data.Options["version"] == "1" {
		o.kv = 1
	}
	return o.kv
}

func escapePath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// read fetches a secret's fields from the store. Called with o.mu held.
func (o *OpenBao) read(ctx context.Context, path string) (map[string]any, int, error) {
	if err := o.ensure(ctx); err != nil {
		return nil, 0, err
	}
	if o.kvVersion(ctx) == 1 {
		var r struct {
			Data map[string]any `json:"data"`
		}
		err := o.call(ctx, http.MethodGet, o.set.Mount+"/"+escapePath(path), o.token, nil, &r)
		return r.Data, 0, err
	}
	var r struct {
		Data struct {
			Data     map[string]any `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	err := o.call(ctx, http.MethodGet, o.set.Mount+"/data/"+escapePath(path), o.token, nil, &r)
	return r.Data.Data, r.Data.Metadata.Version, err
}

// ErrNotFound is a secret or a field that is not there.
var ErrNotFound = errors.New("not found")

// Get returns a value, read again when the last read is older than MaxAge.
// When the store fails and an earlier value exists, that one is returned
// with the error noted in Health only.
func (o *OpenBao) Get(ctx context.Context, ref Ref) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.cache[ref]
	if ok && o.now().Sub(c.read) < o.MaxAge {
		return c.value, nil
	}
	v, version, err := o.fetch(ctx, ref)
	if err != nil {
		if ok {
			return c.value, nil // the last good value
		}
		return "", err
	}
	o.cache[ref] = cached{v, version, o.now()}
	return v, nil
}

// fetch reads one value and records the store's health. Called with o.mu held.
func (o *OpenBao) fetch(ctx context.Context, ref Ref) (string, int, error) {
	data, version, err := o.read(ctx, ref.Path)
	o.note(err)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return "", 0, fmt.Errorf("secret %s: %w", ref.Path, ErrNotFound)
	case err != nil:
		return "", 0, err
	}
	raw, ok := data[ref.Field]
	if !ok {
		return "", version, fmt.Errorf("secret %s has no field %s: %w", ref.Path, ref.Field, ErrNotFound)
	}
	s, ok := raw.(string)
	if !ok {
		return "", version, fmt.Errorf("field %s of secret %s is not text", ref.Field, ref.Path)
	}
	return s, version, nil
}

// note records whether the store answered. Called with o.mu held.
func (o *OpenBao) note(err error) {
	var ae *apiError
	h := Health{Up: err == nil || (errors.As(err, &ae) && ae.Status == http.StatusNotFound), Checked: o.now(), TokenExpiry: o.expiry,
		Renewable: o.renew, Policies: o.policies, KVVersion: o.kv}
	if err != nil && !h.Up {
		h.Error = err.Error()
	}
	o.health = h
}

// Health is the store as last seen.
func (o *OpenBao) Health() Health {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.health
}

// Refresh keeps the login alive and records health; the server calls it
// every minute.
func (o *OpenBao) Refresh(ctx context.Context) Health {
	o.mu.Lock()
	defer o.mu.Unlock()
	err := o.ensure(ctx)
	if err == nil && !o.expiry.IsZero() && !o.renew {
		err = o.lookup(ctx) // its real time left
	}
	o.note(err)
	return o.health
}

// Status of one value, for the interface: never the value itself.
type Status struct {
	Found   bool   `json:"found"`
	Version int    `json:"version,omitempty"`
	Hint    string `json:"hint,omitempty"` // the last four characters, to tell keys apart
	Error   string `json:"error,omitempty"`
}

// Describe reads a value fresh and says whether it is there.
func (o *OpenBao) Describe(ctx context.Context, ref Ref) Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, version, err := o.fetch(ctx, ref)
	if err != nil {
		return Status{Error: err.Error()}
	}
	o.cache[ref] = cached{v, version, o.now()}
	st := Status{Found: true, Version: version}
	if len(v) >= 12 {
		st.Hint = "…" + v[len(v)-4:]
	}
	return st
}

// Capabilities says what vigil may do: on the given secrets, and listing.
type Capabilities struct {
	Read  map[string]bool `json:"read"`
	List  bool            `json:"list"`
	Write bool            `json:"write"`
}

// Test logs in afresh and reports what the login may do.
func (o *OpenBao) Test(ctx context.Context, paths []string) (Health, Capabilities, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.token, o.kv = "", 0
	caps := Capabilities{Read: map[string]bool{}}
	err := o.login(ctx)
	if err == nil && o.set.Auth == "approle" {
		err = o.lookup(ctx)
	}
	o.note(err)
	if err != nil {
		return o.health, caps, err
	}
	kv := o.kvVersion(ctx)
	o.health.KVVersion = kv
	dataPath := func(p string) string {
		if kv == 1 {
			return o.set.Mount + "/" + strings.Trim(p, "/")
		}
		return o.set.Mount + "/data/" + strings.Trim(p, "/")
	}
	listPath := o.set.Mount + "/"
	if kv == 2 {
		listPath = o.set.Mount + "/metadata/"
	}
	all := []string{listPath, dataPath("vigil-capability-probe")}
	for _, p := range paths {
		all = append(all, dataPath(p))
	}
	var r map[string]any
	if err := o.call(ctx, http.MethodPost, "sys/capabilities-self", o.token, map[string]any{"paths": all}, &r); err != nil {
		return o.health, caps, fmt.Errorf("logged in, but the capabilities could not be read: %w", err)
	}
	has := func(path, c string) bool {
		list, _ := r[path].([]any)
		for _, x := range list {
			if x == c || x == "root" {
				return true
			}
		}
		return false
	}
	caps.List = has(listPath, "list")
	caps.Write = has(dataPath("vigil-capability-probe"), "create") || has(dataPath("vigil-capability-probe"), "update")
	for _, p := range paths {
		caps.Read[p] = has(dataPath(p), "read")
	}
	return o.health, caps, nil
}

// List names the secrets under a prefix of the mount, folders ending in /.
func (o *OpenBao) List(ctx context.Context, prefix string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.ensure(ctx); err != nil {
		return nil, err
	}
	path := o.set.Mount + "/"
	if o.kvVersion(ctx) == 2 {
		path = o.set.Mount + "/metadata/"
	}
	if p := strings.Trim(prefix, "/"); p != "" {
		path += escapePath(p) + "/"
	}
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	err := o.call(ctx, "LIST", path, o.token, nil, &r)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(r.Data.Keys)
	return r.Data.Keys, nil
}

// Put writes one field of a secret, keeping its other fields (KV v2 patch
// would need another capability, so this reads and writes the whole secret).
func (o *OpenBao) Put(ctx context.Context, ref Ref, value string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, _, err := o.read(ctx, ref.Path)
	var ae *apiError
	if err != nil && !(errors.As(err, &ae) && ae.Status == http.StatusNotFound) {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	data[ref.Field] = value
	if o.kvVersion(ctx) == 1 {
		err = o.call(ctx, http.MethodPost, o.set.Mount+"/"+escapePath(ref.Path), o.token, data, nil)
	} else {
		err = o.call(ctx, http.MethodPost, o.set.Mount+"/data/"+escapePath(ref.Path), o.token, map[string]any{"data": data}, nil)
	}
	if err == nil {
		delete(o.cache, ref)
	}
	return err
}
