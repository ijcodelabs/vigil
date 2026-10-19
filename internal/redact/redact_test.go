package redact

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

func TestBuiltin(t *testing.T) {
	r, err := New(config.Redact{})
	if err != nil {
		t.Fatal(err)
	}
	// Made-up values in the shapes of real ones. The key headers are put
	// together here so that the secret scanner in CI does not stop at them.
	begin, end := "-----BEGIN ", " PRIVATE "+"KEY-----"
	cases := []struct{ in, want string }{
		{"DB_PASSWORD=example-not-a-secret", "DB_PASSWORD=[redacted]"},
		{`{"api_key": "example-value"}`, `{"api_key": "[redacted]"}`},
		{"export GITHUB_TOKEN=xyz12345 && make", "export GITHUB_TOKEN=[redacted] && make"},
		{"client_secret: 0123456789", "client_secret: [redacted]"},
		{"key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA end", "key sk-ant-[redacted] end"},
		{"OPENAI sk-proj-AAAAAAAAAAAAAAAAAAAAAAAAAAAA", "OPENAI sk-[redacted]"},
		{"ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "ghp_[redacted]"},
		{"id AKIAAAAAAAAAAAAAAAAA used", "id AKIA[redacted] used"},
		{"Authorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAA", "Authorization: Bearer [redacted]"},
		{"postgres://app:hunter2hunter2@db.example.com:5432/shop", "postgres://app:[redacted]@db.example.com:5432/shop"},
		{begin + "OPENSSH" + end + "\nAAAA\nBBBB\n-----END OPENSSH" + end + "\nrest", "[redacted]\nrest"},
		{begin + "RSA" + end + "\nAAAA cut off", "[redacted]"},
		// Left alone: counts and settings that only sound like secrets.
		{"input_tokens=1225 output_tokens=22", "input_tokens=1225 output_tokens=22"},
		{"password_policy=strict, author: someone", "password_policy=strict, author: someone"},
		{"token: 42", "token: 42"},
		{"GET /health 200", "GET /health 200"},
	}
	for _, c := range cases {
		if got := r.String(c.in); got != c.want {
			t.Errorf("%q\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRules(t *testing.T) {
	off, on := false, true
	r, err := New(config.Redact{Builtin: &off, Rules: []config.RedactRule{
		{Name: "card", Match: `\b(\d{4})\d{8}(\d{4})\b`, Replace: "$1********$2"},
		{Name: "customer", Match: `customer=\S+`},
		{Name: "unused", Match: `.`, Enabled: &off},
	}})
	if err != nil {
		t.Fatal(err)
	}
	batch := []model.LogEntry{{Msg: "paid with 4111111111111111 by customer=ann@example.com",
		Stream: map[string]string{"app": "customer=shop"}, Fields: map[string]string{"pw": "DB_PASSWORD=example-not-a-secret"}}}
	r.Entries(batch)
	if want := "paid with 4111********1111 by [redacted]"; batch[0].Msg != want {
		t.Errorf("msg %q, want %q", batch[0].Msg, want)
	}
	if batch[0].Fields["pw"] != "DB_PASSWORD=example-not-a-secret" {
		t.Errorf("built-in rules ran while switched off: %q", batch[0].Fields["pw"])
	}
	if batch[0].Stream["app"] != "customer=shop" {
		t.Errorf("stream labels are not masked: %v", batch[0].Stream)
	}

	r, _ = New(config.Redact{Builtin: &on})
	batch = []model.LogEntry{{Msg: "ok", Fields: map[string]string{"pw": "DB_PASSWORD=example-not-a-secret"}}}
	r.Entries(batch)
	if batch[0].Fields["pw"] != "DB_PASSWORD=[redacted]" {
		t.Errorf("field %q", batch[0].Fields["pw"])
	}
	var none *Redactor
	if none.String("DB_PASSWORD=example-not-a-secret") == "" || none.Active() {
		t.Error("a nil redactor changes nothing")
	}
}
