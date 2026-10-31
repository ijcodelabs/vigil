package opscli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstToken(t *testing.T) {
	dir := t.TempDir()
	for text, want := range map[string]string{
		"just-the-token\n": "just-the-token",
		"# senders\n\nshop tok-a   # the shop\nbilling tok-b\n": "tok-a",
	} {
		p := filepath.Join(dir, "t")
		os.WriteFile(p, []byte(text), 0o600)
		if got, err := firstToken(p); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", text, got, err, want)
		}
	}
	p := filepath.Join(dir, "empty")
	os.WriteFile(p, []byte("# nothing\n"), 0o600)
	if _, err := firstToken(p); err == nil {
		t.Error("a file without a token was accepted")
	}
}
