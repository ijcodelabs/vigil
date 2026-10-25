package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPI answers like the Messages API and keeps the last request.
func fakeAPI(t *testing.T, stop, text string, got *map[string]any, header *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, got)
		*header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"stop_reason": stop, "content": []any{map[string]any{"type": "text", "text": text}},
			"usage": map[string]any{"input_tokens": 1200, "output_tokens": 300, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 0}})
	}))
}

func keyFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "key")
	os.WriteFile(p, []byte("test-key-not-real\n"), 0o600)
	return p
}

func TestAnthropicRequestAndAnswer(t *testing.T) {
	var got map[string]any
	var h http.Header
	api := fakeAPI(t, "end_turn", `{"summary":"ok"}`, &got, &h)
	defer api.Close()
	p := NewAnthropic("claude-opus-5-5", "low", FileKey(keyFile(t)), api.URL)
	res, err := p.Complete(context.Background(), Request{System: "rules", User: "question",
		Schema: map[string]any{"type": "object"}, MaxTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON) != `{"summary":"ok"}` || res.Usage.Input != 1200 || res.Usage.Output != 300 || res.Usage.CacheRead != 50 || res.Model != "claude-opus-5-5" {
		t.Errorf("response: %+v", res)
	}
	oc, _ := got["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if got["model"] != "claude-opus-5-5" || oc["effort"] != "low" || format["type"] != "json_schema" || got["fallbacks"] != "default" {
		t.Errorf("request: %v", got)
	}
	if h.Get("X-Api-Key") != "test-key-not-real" || !strings.Contains(h.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("headers: key %q beta %q", h.Get("X-Api-Key"), h.Get("Anthropic-Beta"))
	}
}

func TestAnthropicStops(t *testing.T) {
	for stop, want := range map[string]string{"refusal": "declined", "max_tokens": "cut off"} {
		var got map[string]any
		var h http.Header
		api := fakeAPI(t, stop, "", &got, &h)
		_, err := NewAnthropic("claude-haiku-5-5", "low", FileKey(keyFile(t)), api.URL).Complete(context.Background(), Request{MaxTokens: 100, Schema: map[string]any{}})
		api.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", stop, err)
		}
		if stop == "refusal" && !errors.Is(err, ErrRefused) {
			t.Errorf("refusal is not ErrRefused: %v", err)
		}
		if got["fallbacks"] != nil {
			t.Errorf("fallbacks sent for a model without them: %v", got["fallbacks"])
		}
	}
}

func TestAnthropicMissingKey(t *testing.T) {
	_, err := NewAnthropic("claude-opus-5-5", "low", FileKey("/nonexistent/key"), "http://127.0.0.1:1").Complete(context.Background(), Request{MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("missing key: %v", err)
	}
}

// The server's environment does not choose the key or the address.
func TestAnthropicIgnoresEnvironment(t *testing.T) {
	var got map[string]any
	var h http.Header
	api := fakeAPI(t, "end_turn", `{}`, &got, &h)
	defer api.Close()
	t.Setenv("ANTHROPIC_API_KEY", "env-key-must-not-be-used")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	if _, err := NewAnthropic("claude-haiku-5-5", "low", FileKey(keyFile(t)), api.URL).Complete(context.Background(), Request{MaxTokens: 10, Schema: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if h.Get("X-Api-Key") != "test-key-not-real" {
		t.Errorf("key used: %q", h.Get("X-Api-Key"))
	}
}
