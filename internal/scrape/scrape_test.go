package scrape

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A token file is read on every scrape, so a new token is used at once, and
// an unreadable one fails the scrape instead of sending none.
func TestBearerTokenFile(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.WriteString(w, "up 1\n")
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("first-test-value\n"), 0o600)
	c := NewClient(time.Second)
	if _, err := c.Scrape(context.Background(), Request{URL: srv.URL, BearerTokenFile: file}); err != nil || got != "Bearer first-test-value" {
		t.Fatalf("first: %q %v", got, err)
	}
	os.WriteFile(file, []byte("second-test-value"), 0o600)
	if _, err := c.Scrape(context.Background(), Request{URL: srv.URL, BearerTokenFile: file}); err != nil || got != "Bearer second-test-value" {
		t.Fatalf("after a change: %q %v", got, err)
	}
	if _, err := c.Scrape(context.Background(), Request{URL: srv.URL, BearerTokenFile: file + ".missing"}); err == nil || !strings.Contains(err.Error(), "bearer_token_file") {
		t.Fatalf("missing file: %v", err)
	}
}
