package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"

	sessionCookie = "vigil_session"
	sessionTTL    = 7 * 24 * time.Hour
	// csrfHeader must be present on every state-changing request that is
	// authenticated by cookie. A browser will not let another site add it.
	csrfHeader = "X-Vigil-CSRF"

	maxLoginFailures   = 10 // per address per window
	maxAccountFailures = 20 // per account name per window, from anywhere
	loginWindow        = 5 * time.Minute
)

var userNameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

type principalKey struct{}

type principal struct {
	User string `json:"user"`
	Role string `json:"role"`
	// Bootstrap is true for the built-in admin that exists only while no
	// user accounts have been created.
	Bootstrap bool `json:"bootstrap,omitempty"`
	// ReadOnly is true for a caller that came with a read-only token: it
	// sees what a viewer sees and changes nothing.
	ReadOnly bool `json:"read_only,omitempty"`
	// session is the id of the session the caller signed in with, if any.
	session string
}

func bearer(r *http.Request) string {
	t, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return t
}

func sameToken(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// identify works out who is calling: the admin token (scripts, amtool) or
// a signed-in user's session cookie. viaCookie tells the two apart.
func (s *Server) identify(r *http.Request) (p *principal, viaCookie bool) {
	if sameToken(bearer(r), s.cfg.AdminToken) {
		return &principal{User: "admin-token", Role: RoleAdmin}, false
	}
	if t := bearer(r); strings.HasPrefix(t, readTokenPrefix) {
		if name, ok := s.readToken(t); ok {
			return &principal{User: "token " + name, Role: RoleViewer, ReadOnly: true}, false
		}
		return nil, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	user, err := s.store.SessionUser(c.Value)
	if err != nil || user == "" {
		return nil, false
	}
	if user == bootstrapUser {
		if s.hasUsers() {
			return nil, false // real accounts exist now; the bootstrap login is over
		}
		return &principal{User: "admin", Role: RoleAdmin, Bootstrap: true}, true
	}
	_, role, found, err := s.store.UserAuth(user)
	if err != nil || !found {
		return nil, false
	}
	return &principal{User: user, Role: role, session: c.Value}, true
}

// bootstrapUser marks a session opened with the admin token before any
// user account existed. The name cannot collide with a real user.
const bootstrapUser = "\x00bootstrap"

func (s *Server) hasUsers() bool {
	users, err := s.store.Users()
	return err != nil || len(users) > 0
}

// require wraps a handler so only the given role (or admin) may call it.
// asks says that the handler only answers questions whatever the method:
// query APIs take their query by POST as well as by GET.
func (s *Server) require(role string, asks bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, viaCookie := s.identify(r)
		if p == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if role == RoleAdmin && p.Role != RoleAdmin {
			http.Error(w, "this needs an admin account", http.StatusForbidden)
			return
		}
		if p.ReadOnly && !asks && r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "this token only reads", http.StatusForbidden)
			return
		}
		if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		// Credentials stop here; nothing behind the server needs them.
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func (s *Server) viewer(next http.HandlerFunc) http.HandlerFunc {
	return s.require(RoleViewer, false, next)
}
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.require(RoleAdmin, false, next)
}

// asks is viewer for the query APIs: a POST there changes nothing, so a
// read-only token may send one.
func (s *Server) asks(next http.HandlerFunc) http.HandlerFunc {
	return s.require(RoleViewer, true, next)
}

// ingest lets agents in: with the shared ingest token (any agent id), or
// with a token issued to one agent, which only works for that agent's id.
func (s *Server) ingest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !sameToken(tok, s.cfg.IngestToken) {
			owner := ""
			if tok != "" {
				owner = s.tokenOwner(hashToken(tok))
			}
			if owner == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Vigil-Agent") != owner {
				http.Error(w, "this token belongs to a different agent", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// loginLimiter slows password guessing. Failures are counted per client
// address and per account name: the first stops one machine trying many
// accounts, the second many machines trying one account. A key is blocked
// while it has too many recent failures.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (l *loginLimiter) recent(key string) int {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if time.Since(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = kept
	}
	return len(kept)
}

func (l *loginLimiter) blocked(addr string) bool { return l.blockedAt(addr, maxLoginFailures) }

func (l *loginLimiter) blockedAt(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recent(key) >= limit
}

func (l *loginLimiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 50000 { // someone is spraying names or addresses: forget the idle ones
		for k := range l.fails {
			l.recent(k)
		}
	}
	for _, k := range keys {
		l.fails[k] = append(l.fails[k], time.Now())
	}
}

// clientAddr is the address of whoever is really calling. Behind a reverse
// proxy the connection comes from the proxy, and the client's address is in
// X-Forwarded-For; that header is believed only when the connection comes
// from a proxy named in trusted_proxies, and read from the right, skipping
// further trusted proxies, since everything left of that the client wrote.
func (s *Server) clientAddr(r *http.Request) string {
	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		addr = r.RemoteAddr
	}
	if !s.trusted(addr) {
		return addr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if net.ParseIP(hop) == nil {
			continue
		}
		if !s.trusted(hop) {
			return hop
		}
	}
	return addr
}

func (s *Server) trusted(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, n := range s.proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// viaTLS reports whether the client's connection is encrypted: to us
// directly, or to a trusted proxy that says so.
func (s *Server) viaTLS(r *http.Request) bool {
	if r.TLS != nil && len(s.proxies) == 0 {
		return true
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.trusted(addr) {
		return r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(s.cfg.ExternalURL, "https://")
	}
	return r.TLS != nil
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			s.store.DeleteSession(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		p, _ := s.identify(r)
		if p == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("POST /api/v1/me/password", s.viewer(s.handleOwnPassword))
	mux.HandleFunc("GET /api/v1/users", s.admin(func(w http.ResponseWriter, _ *http.Request) {
		users, err := s.store.Users()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, users)
	}))
	mux.HandleFunc("PUT /api/v1/users/{name}", s.admin(s.handlePutUser))
	mux.HandleFunc("DELETE /api/v1/users/{name}", s.admin(s.handleDeleteUser))
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	// A sign-in has to come from the interface (or a script that says so):
	// a form on another site cannot set this header, so it cannot sign a
	// visitor in to an account of the attacker's choosing.
	if r.Header.Get(csrfHeader) == "" {
		http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
		return
	}
	if len(in.Username) > 200 || len(in.Password) > 200 {
		http.Error(w, "wrong user name or password", http.StatusUnauthorized)
		return
	}
	addr, account := s.clientAddr(r), "user:"+strings.ToLower(in.Username)
	if s.logins.blocked(addr) || s.logins.blockedAt(account, maxAccountFailures) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "too many failed sign-ins, try again in a few minutes", http.StatusTooManyRequests)
		return
	}
	user := ""
	hash, _, found, err := s.store.UserAuth(in.Username)
	if !found {
		// Take as long as for an account that exists, so the time an
		// answer takes does not tell which names are real.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
	}
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	case found && bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) == nil:
		user = in.Username
	case !found && in.Username == "admin" && !s.hasUsers() && sameToken(in.Password, s.cfg.AdminToken):
		// First sign-in on a fresh server: the admin token is the password.
		user = bootstrapUser
	}
	if user == "" {
		s.logins.fail(addr, account)
		s.log.Warn("failed sign-in", "user", in.Username, "from", addr)
		http.Error(w, "wrong user name or password", http.StatusUnauthorized)
		return
	}
	id := randomToken()
	if err := s.store.CreateSession(id, user, time.Now().Add(sessionTTL)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: s.viaTLS(r)})
	w.WriteHeader(http.StatusNoContent)
}

// dummyHash is compared against when the account does not exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("no such account"), bcrypt.DefaultCost)

func checkPassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	if len(pw) > 72 {
		return errors.New("password must be at most 72 characters")
	}
	return nil
}

func (s *Server) handlePutUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct{ Password, Role string }
	if !readJSON(w, r, &in) {
		return
	}
	if !userNameRe.MatchString(name) {
		http.Error(w, "user names may use letters, digits and . _ @ -", http.StatusBadRequest)
		return
	}
	if in.Role != RoleAdmin && in.Role != RoleViewer {
		http.Error(w, "role must be admin or viewer", http.StatusBadRequest)
		return
	}
	_, oldRole, exists, _ := s.store.UserAuth(name)
	if !exists && in.Password == "" {
		http.Error(w, "a new user needs a password", http.StatusBadRequest)
		return
	}
	if !exists && !s.hasUsers() && in.Role != RoleAdmin {
		http.Error(w, "the first account must be an admin", http.StatusBadRequest)
		return
	}
	if exists && oldRole == RoleAdmin && in.Role != RoleAdmin && s.lastAdmin(name) {
		http.Error(w, "this is the only admin; make another admin first", http.StatusConflict)
		return
	}
	hash := ""
	if in.Password != "" {
		if err := checkPassword(in.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hash = string(h)
	}
	if err := s.store.PutUser(name, hash, in.Role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A new password or another role ends the user's sessions: the old
	// password may be known to someone, and a session keeps its role.
	if exists && (hash != "" || oldRole != in.Role) {
		if err := s.store.DeleteUserSessions(name, ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// lastAdmin reports whether name is the only admin account.
func (s *Server) lastAdmin(name string) bool {
	users, _ := s.store.Users()
	for _, u := range users {
		if u.Role == RoleAdmin && u.Name != name {
			return false
		}
	}
	return true
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, role, found, _ := s.store.UserAuth(name); found && role == RoleAdmin && s.lastAdmin(name) {
		http.Error(w, "this is the only admin; make another admin first", http.StatusConflict)
		return
	}
	if err := s.store.DeleteUser(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOwnPassword lets a signed-in user change their own password.
func (s *Server) handleOwnPassword(w http.ResponseWriter, r *http.Request) {
	p, _ := r.Context().Value(principalKey{}).(*principal)
	var in struct{ Old, New string }
	if !readJSON(w, r, &in) {
		return
	}
	hash, role, found, _ := s.store.UserAuth(p.User)
	if !found {
		http.Error(w, "this sign-in has no password of its own; create a user account first", http.StatusBadRequest)
		return
	}
	// Guessing the current password from a borrowed session is limited too.
	account := "user:" + strings.ToLower(p.User)
	if s.logins.blockedAt(account, maxAccountFailures) {
		http.Error(w, "too many wrong attempts, try again in a few minutes", http.StatusTooManyRequests)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Old)) != nil {
		s.logins.fail(account)
		http.Error(w, "current password is wrong", http.StatusForbidden)
		return
	}
	if err := checkPassword(in.New); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err == nil {
		err = s.store.PutUser(p.User, string(h), role)
	}
	if err == nil {
		// Whoever else is signed in as this user is signed out: a changed
		// password is how one gets rid of a session one does not trust.
		err = s.store.DeleteUserSessions(p.User, p.session)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
