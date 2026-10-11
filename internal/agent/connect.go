package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/pki"
)

// HTTPClient returns a client that trusts the server the way the config
// says: a certificate file, the fingerprint of the server's own authority,
// or the system's authorities.
func HTTPClient(cfg *config.Agent) (*http.Client, error) {
	tc, err := pki.ClientTLS(pki.ClientOptions{ServerURL: cfg.Server.URL, CAFile: cfg.Server.CAFile, CAPin: cfg.Server.CAPin,
		CacheFile: filepath.Join(cfg.SpoolDir, "ca.pem"), Insecure: cfg.Server.TLSInsecure})
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

// Enroll exchanges an enrolment token for a token that belongs to agentID.
// permanent is true when the server refused in a way that trying again will
// not change.
func Enroll(ctx context.Context, hc *http.Client, serverURL, enrollToken, agentID string) (token string, permanent bool, err error) {
	body, _ := json.Marshal(map[string]string{"token": enrollToken, "agent_id": agentID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return "", true, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound,
			fmt.Errorf("the server refused the enrolment (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return "", true, fmt.Errorf("unexpected answer to the enrolment: %.200s", b)
	}
	return out.Token, false, nil
}

// connect prepares what every request to the server needs: a client that
// trusts the server, and this agent's token. It keeps trying until both are
// there; collection runs meanwhile and fills the spool. An enrolled token is
// kept in the spool directory, so the enrolment token is needed only once.
func (a *Agent) connect(ctx context.Context) {
	defer close(a.ready)
	tokenFile := filepath.Join(a.cfg.SpoolDir, "agent-token")
	wait, said := time.Second, ""
	fail := func(msg string, err error) bool {
		if s := msg + err.Error(); s != said { // say each distinct problem once
			a.log.Warn(msg+", will keep trying", "err", err)
			said = s
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
		return true
	}
	for {
		hc, err := HTTPClient(a.cfg)
		if err != nil {
			if !fail("cannot set up the encrypted connection to the server", err) {
				return
			}
			continue
		}
		a.http = hc
		if a.cfg.Server.Token != "" {
			return
		}
		if b, err := os.ReadFile(tokenFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			a.cfg.Server.Token = string(bytes.TrimSpace(b))
			return
		}
		token, permanent, err := Enroll(ctx, hc, a.cfg.Server.URL, a.cfg.Server.EnrollToken, a.cfg.AgentID)
		if err != nil {
			if permanent {
				wait = max(wait, time.Minute) // a refusal counts against this address on the server
			}
			if !fail("cannot enrol with the server", err) {
				return
			}
			continue
		}
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			// Without the file the next start would enrol again and be
			// refused, so say it loudly; this run still works.
			a.log.Error("enrolled, but the token could not be saved; the agent will not be able to start again until this is fixed", "file", tokenFile, "err", err)
		}
		a.cfg.Server.Token = token
		a.log.Info("enrolled with the server", "agent", a.cfg.AgentID)
		return
	}
}

// waitReady blocks until connect is done; false means the agent is stopping.
func (a *Agent) waitReady(ctx context.Context) bool {
	select {
	case <-a.ready:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
