package scrape

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
)

const maxBody = 64 << 20

// Client scrapes targets. The zero value is not usable; call NewClient.
type Client struct {
	secure, insecure *http.Client
}

// NewClient makes a client whose requests are bounded by their context;
// maxTimeout is only a backstop.
func NewClient(maxTimeout time.Duration) *Client {
	timeout := maxTimeout
	mk := func(skipVerify bool) *http.Client {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: skipVerify}
		return &http.Client{Timeout: timeout, Transport: tr}
	}
	return &Client{secure: mk(false), insecure: mk(true)}
}

// Request is one scrape: where, and how to authenticate.
type Request struct {
	URL                string
	Insecure           bool // skip TLS verification
	BearerToken        string
	BearerTokenFile    string // read now, so that a new token needs no restart
	Username, Password string
}

// Scrape fetches the target and returns its samples stamped with the fetch
// time. The context carries the timeout.
func (c *Client) Scrape(ctx context.Context, r Request) ([]model.Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	if r.BearerTokenFile != "" {
		tok, err := os.ReadFile(r.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading bearer_token_file: %w", err)
		}
		r.BearerToken = strings.TrimSpace(string(tok))
	}
	switch {
	case r.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+r.BearerToken)
	case r.Username != "":
		req.SetBasicAuth(r.Username, r.Password)
	}
	hc := c.secure
	if r.Insecure {
		hc = c.insecure
	}
	ts := time.Now().UnixMilli()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return Parse(io.LimitReader(resp.Body, maxBody), ts)
}
