package otlp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// tokenFile holds the emitters allowed to send, read from a file of
// "name token" lines. The file is read again when it changes, so a token
// is added or revoked without a restart; a file that cannot be read keeps
// the tokens from before, and with none from before nobody gets in.
type tokenFile struct {
	path string

	mu      sync.Mutex
	checked time.Time
	mod     time.Time
	size    int64
	tokens  map[string]string // token → emitter name
	err     error
}

// tokenRecheck is how often the file's modification time is looked at.
const tokenRecheck = 5 * time.Second

func newTokenFile(path string) (*tokenFile, error) {
	t := &tokenFile{path: path}
	t.load(time.Now())
	return t, t.err
}

// emitter returns the name the bearer token in the request belongs to.
func (t *tokenFile) emitter(req *http.Request, now time.Time) (string, bool) {
	given, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || given == "" {
		return "", false
	}
	t.mu.Lock()
	if now.Sub(t.checked) >= tokenRecheck {
		t.load(now)
	}
	tokens := t.tokens
	t.mu.Unlock()
	// Every token is compared, so the time taken does not tell which
	// one was close.
	name := ""
	for token, n := range tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(given)) == 1 {
			name = n
		}
	}
	return name, name != ""
}

// load reads the file if it changed; the caller holds mu (or is newTokenFile).
func (t *tokenFile) load(now time.Time) {
	t.checked = now
	st, err := os.Stat(t.path)
	if err != nil {
		t.err = err
		return
	}
	if t.tokens != nil && st.ModTime().Equal(t.mod) && st.Size() == t.size {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		t.err = err
		return
	}
	tokens, err := parseTokens(b)
	if err != nil {
		t.err = fmt.Errorf("%s: %w", t.path, err)
		return
	}
	t.tokens, t.mod, t.size, t.err = tokens, st.ModTime(), st.Size(), nil
}

// parseTokens reads "name token" lines; blank lines and # comments are
// skipped. A name may hold several tokens (for a rotation), a token only
// one name.
func parseTokens(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("line %d: want \"name token\"", n)
		}
		name, token := f[0], f[1]
		if !validEmitter(name) {
			return nil, fmt.Errorf("line %d: the name may hold only a-z, 0-9, '.', '_' and '-'", n)
		}
		if len(token) < 16 {
			return nil, fmt.Errorf("line %d: the token for %s is shorter than 16 characters", n, name)
		}
		if other, dup := out[token]; dup && other != name {
			return nil, fmt.Errorf("line %d: the token for %s is also %s's", n, name, other)
		}
		out[token] = name
	}
	return out, sc.Err()
}

func validEmitter(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type emitterKey struct{}

// requireToken lets through only requests with a token from the file, and
// puts the emitter's name into the request's context.
func requireToken(t *tokenFile, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name, ok := t.emitter(req, time.Now())
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vigil-otlp"`)
			http.Error(w, "a valid bearer token is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), emitterKey{}, name)))
	})
}

func emitterOf(req *http.Request) string {
	name, _ := req.Context().Value(emitterKey{}).(string)
	return name
}

// keyPair serves a certificate from files, read again when they change, so
// a renewed certificate is picked up without a restart.
type keyPair struct {
	cert, key string

	mu   sync.Mutex
	mod  time.Time
	last *tls.Certificate
}

func (k *keyPair) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cs, err1 := os.Stat(k.cert)
	ks, err2 := os.Stat(k.key)
	if err1 != nil || err2 != nil {
		if k.last != nil {
			return k.last, nil
		}
		return nil, fmt.Errorf("OTLP certificate: %w", errors.Join(err1, err2))
	}
	mod := cs.ModTime()
	if ks.ModTime().After(mod) {
		mod = ks.ModTime()
	}
	if k.last != nil && mod.Equal(k.mod) {
		return k.last, nil
	}
	c, err := tls.LoadX509KeyPair(k.cert, k.key)
	if err != nil {
		if k.last != nil {
			return k.last, nil // half-written during a renewal: keep the old one
		}
		return nil, fmt.Errorf("OTLP certificate: %w", err)
	}
	k.last, k.mod = &c, mod
	return k.last, nil
}
