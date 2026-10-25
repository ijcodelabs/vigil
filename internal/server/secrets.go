package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/secrets"
)

// The secret store is set up in the interface (Settings > Secrets): where
// an OpenBao (or Vault) is, how vigil logs in, and which secret each thing
// vigil needs comes from. The settings are kept in the settings table; the
// secret half of the login (secret_id or token) is kept there sealed with
// a key file in data_dir (secrets.key), so the database alone reveals
// nothing. Values are never sent back by any API: the interface only learns
// whether a value is there, its version, and its last four characters.

const (
	settingStore     = "secret_store"
	settingStoreCred = "secret_store_cred"
	settingRefs      = "secret_refs"
)

// secretUse is one thing vigil reads from the store.
type secretUse struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	UsedBy string `json:"used_by"`
}

var secretUses = []secretUse{
	{"ai_api_key", "AI provider key", "Explain on the Home page and in Explore"},
}

type vault struct {
	mu     sync.Mutex
	sealer *secrets.Sealer
	store  *secrets.OpenBao // nil: no store set up
	refs   map[string]secrets.Ref
}

// openVault reads the saved store settings at start. A store that cannot be
// opened is logged and left out; the server runs without it.
func (s *Server) openVault() {
	s.vault.refs = map[string]secrets.Ref{}
	if raw, err := s.store.Setting(settingRefs, ""); err == nil && raw != "" {
		json.Unmarshal([]byte(raw), &s.vault.refs)
	}
	raw, err := s.store.Setting(settingStore, "")
	if err != nil || raw == "" {
		return
	}
	var set secrets.Settings
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		s.log.Warn("the saved secret store settings are unreadable", "err", err)
		return
	}
	cred, err := s.storeCredential()
	if err == nil {
		s.vault.store, err = secrets.New(set, cred)
	}
	if err != nil {
		s.log.Warn("the secret store is not used", "err", err)
	}
}

func (s *Server) sealer() (*secrets.Sealer, error) {
	if s.vault.sealer == nil {
		sl, err := secrets.OpenSealer(filepath.Join(s.cfg.DataDir, "secrets.key"))
		if err != nil {
			return nil, err
		}
		s.vault.sealer = sl
	}
	return s.vault.sealer, nil
}

func (s *Server) storeCredential() (string, error) {
	sealed, err := s.store.Setting(settingStoreCred, "")
	if err != nil || sealed == "" {
		return "", errors.New("no secret_id or token is saved")
	}
	sl, err := s.sealer()
	if err != nil {
		return "", err
	}
	return sl.Open(settingStoreCred, sealed)
}

// secret reads a value vigil uses, by its name in secretUses.
func (s *Server) secret(ctx context.Context, name string) (string, error) {
	s.vault.mu.Lock()
	store := s.vault.store
	ref, ok := s.vault.refs[name]
	s.vault.mu.Unlock()
	switch {
	case store == nil:
		return "", errors.New("no secret store is set up (Settings > Secrets)")
	case !ok:
		return "", fmt.Errorf("no secret is chosen for %s (Settings > Secrets)", name)
	}
	return store.Get(ctx, ref)
}

// aiKeySource is where the language model's key comes from: the file in the
// config if there is one, else the secret store.
func (s *Server) aiKeySource() string {
	switch {
	case s.cfg.AI.Provider == "":
		return "off"
	case s.cfg.AI.APIKeyFile != "":
		return "file"
	}
	return "store"
}

// vaultLoop keeps the store's login alive and its health in the metric
// store: vigil_secret_store_up, and how long the login has left.
func (s *Server) vaultLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.vault.mu.Lock()
		store := s.vault.store
		s.vault.mu.Unlock()
		if store != nil {
			h := store.Refresh(ctx)
			now := time.Now()
			up := 0.0
			if h.Up {
				up = 1
			}
			samples := []model.Sample{{Name: "vigil_secret_store_up", Labels: map[string]string{}, Value: up, TS: now.UnixMilli()}}
			if !h.TokenExpiry.IsZero() {
				samples = append(samples, model.Sample{Name: "vigil_secret_store_token_expiry_seconds", Labels: map[string]string{"renewable": fmt.Sprint(h.Renewable)},
					Value: h.TokenExpiry.Sub(now).Seconds(), TS: now.UnixMilli()})
			}
			if err := s.forward(ctx, "", promwire.Encode(samples)); err != nil && ctx.Err() == nil {
				s.log.Warn("writing the secret store's health failed", "err", err)
			}
			if !h.Up {
				s.log.Warn("the secret store does not answer; the last values read are used", "err", h.Error)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// confirmPassword asks for the signed-in person's password again before a
// change to the secret store: a borrowed session is not enough.
func (s *Server) confirmPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	p := caller(r)
	account := "user:" + strings.ToLower(p.User)
	if s.logins.blockedAt(account, maxAccountFailures) {
		http.Error(w, "too many wrong attempts, try again in a few minutes", http.StatusTooManyRequests)
		return false
	}
	ok := false
	if p.Bootstrap || p.User == "admin-token" { // signed in with the admin token: it is the password
		ok = sameToken(password, s.cfg.AdminToken)
	} else if hash, _, found, err := s.store.UserAuth(p.User); err == nil && found {
		ok = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	}
	if !ok {
		s.logins.fail(account)
		http.Error(w, "your password is wrong", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) secretRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/secrets", s.admin(s.handleSecrets))
	mux.HandleFunc("POST /api/v1/secrets/store/test", s.admin(s.handleStoreTest))
	mux.HandleFunc("PUT /api/v1/secrets/store", s.admin(s.handleStoreSave))
	mux.HandleFunc("POST /api/v1/secrets/store/remove", s.admin(s.handleStoreRemove))
	mux.HandleFunc("GET /api/v1/secrets/store/list", s.admin(s.handleStoreList))
	mux.HandleFunc("PUT /api/v1/secrets/uses/{name}", s.admin(s.handleUseSave))
	mux.HandleFunc("POST /api/v1/secrets/uses/{name}/test", s.admin(s.handleUseTest))
}

type useView struct {
	secretUse
	Source string          `json:"source"` // store, file (set in the config file), off
	File   string          `json:"file,omitempty"`
	Ref    *secrets.Ref    `json:"ref,omitempty"`
	Status *secrets.Status `json:"status,omitempty"`
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	s.vault.mu.Lock()
	store, refs := s.vault.store, s.vault.refs
	s.vault.mu.Unlock()
	out := map[string]any{"store": nil, "has_credential": false}
	if c, _ := s.store.Setting(settingStoreCred, ""); c != "" {
		out["has_credential"] = true
	}
	uses := []useView{}
	for _, u := range secretUses {
		v := useView{secretUse: u, Source: "store"}
		if u.Name == "ai_api_key" {
			v.Source = s.aiKeySource()
			v.File = s.cfg.AI.APIKeyFile
		}
		if ref, ok := refs[u.Name]; ok {
			v.Ref = &ref
			if store != nil && v.Source == "store" {
				st := store.Describe(r.Context(), ref)
				v.Status = &st
			}
		}
		uses = append(uses, v)
	}
	out["uses"] = uses
	if store != nil { // after reading the uses, which is the freshest look at the store
		out["store"], out["health"] = store.Settings(), store.Health()
	}
	writeJSON(w, out)
}

type storeRequest struct {
	Store      secrets.Settings `json:"store"`
	Credential string           `json:"credential"` // empty: keep the saved one, when the store is the same
	Password   string           `json:"password"`
}

// sameStore says whether a saved credential may be used with new settings:
// only for the same store, mount and login. Otherwise anyone able to change
// the address could have vigil send its login elsewhere.
func sameStore(a, b secrets.Settings) bool {
	return a.Address == b.Address && a.Auth == b.Auth && a.RoleID == b.RoleID && a.AuthMount == b.AuthMount && a.Namespace == b.Namespace && a.CACert == b.CACert
}

// storeFor builds a client from a request, with the saved credential when
// none is given and the store is the same.
func (s *Server) storeFor(w http.ResponseWriter, in *storeRequest) (*secrets.OpenBao, bool) {
	if err := in.Store.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	cred := strings.TrimSpace(in.Credential)
	if cred == "" {
		s.vault.mu.Lock()
		cur := s.vault.store
		s.vault.mu.Unlock()
		if cur == nil || !sameStore(cur.Settings(), in.Store) {
			http.Error(w, "enter the secret_id (or token) again: it is only reused for the same store and login", http.StatusBadRequest)
			return nil, false
		}
		var err error
		if cred, err = s.storeCredential(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil, false
		}
	}
	in.Credential = cred
	o, err := secrets.New(in.Store, cred)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return o, true
}

func (s *Server) usedPaths() []string {
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	var paths []string
	for _, ref := range s.vault.refs {
		paths = append(paths, ref.Path)
	}
	return paths
}

func (s *Server) handleStoreTest(w http.ResponseWriter, r *http.Request) {
	var in storeRequest
	if !readJSON(w, r, &in) {
		return
	}
	o, ok := s.storeFor(w, &in)
	if !ok {
		return
	}
	h, caps, err := o.Test(r.Context(), s.usedPaths())
	res := map[string]any{"health": h, "capabilities": caps}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, res)
}

func (s *Server) handleStoreSave(w http.ResponseWriter, r *http.Request) {
	var in storeRequest
	if !readJSON(w, r, &in) || !s.confirmPassword(w, r, in.Password) {
		return
	}
	o, ok := s.storeFor(w, &in)
	if !ok {
		return
	}
	if _, _, err := o.Test(r.Context(), nil); err != nil {
		http.Error(w, "not saved: "+err.Error(), http.StatusBadRequest)
		return
	}
	sl, err := s.sealer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	set, _ := json.Marshal(o.Settings())
	if err = s.store.SetSetting(settingStore, string(set)); err == nil {
		err = s.store.SetSetting(settingStoreCred, sl.Seal(settingStoreCred, in.Credential))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.vault.mu.Lock()
	s.vault.store = o
	s.vault.mu.Unlock()
	s.log.Info("secret store saved", "by", caller(r).User, "address", in.Store.Address, "auth", in.Store.Auth, "mount", in.Store.Mount)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStoreRemove(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password string }
	if !readJSON(w, r, &in) || !s.confirmPassword(w, r, in.Password) {
		return
	}
	err := s.store.SetSetting(settingStore, "")
	if err == nil {
		err = s.store.SetSetting(settingStoreCred, "")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.vault.mu.Lock()
	s.vault.store = nil
	s.vault.mu.Unlock()
	s.log.Info("secret store removed", "by", caller(r).User)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStoreList(w http.ResponseWriter, r *http.Request) {
	s.vault.mu.Lock()
	store := s.vault.store
	s.vault.mu.Unlock()
	if store == nil {
		http.Error(w, "no secret store is set up", http.StatusConflict)
		return
	}
	keys, err := store.List(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, keys)
}

func knownUse(name string) bool {
	for _, u := range secretUses {
		if u.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) handleUseSave(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !knownUse(name) {
		http.NotFound(w, r)
		return
	}
	var ref secrets.Ref
	if !readJSON(w, r, &ref) {
		return
	}
	if err := ref.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.vault.mu.Lock()
	refs := map[string]secrets.Ref{}
	for k, v := range s.vault.refs {
		refs[k] = v
	}
	refs[name] = ref
	raw, _ := json.Marshal(refs)
	err := s.store.SetSetting(settingRefs, string(raw))
	if err == nil {
		s.vault.refs = refs
	}
	s.vault.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("secret chosen", "use", name, "path", ref.Path, "field", ref.Field, "by", caller(r).User)
	w.WriteHeader(http.StatusNoContent)
}

// handleUseTest reads the value fresh and, for the AI key, asks the
// provider whether it accepts it.
func (s *Server) handleUseTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !knownUse(name) {
		http.NotFound(w, r)
		return
	}
	s.vault.mu.Lock()
	store := s.vault.store
	ref, ok := s.vault.refs[name]
	s.vault.mu.Unlock()
	if store == nil || !ok {
		http.Error(w, "set up the store and choose the secret first", http.StatusConflict)
		return
	}
	res := map[string]any{"status": store.Describe(r.Context(), ref)}
	if name == "ai_api_key" && s.ai != nil && s.aiKeySource() == "store" {
		if err := s.ai.Verify(r.Context()); err != nil {
			res["provider"] = err.Error()
		} else {
			res["provider"] = "accepted"
		}
	}
	writeJSON(w, res)
}
