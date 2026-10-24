// Package ai is vigil's door to a language model. One interface, so that
// the provider can change without the rest of vigil noticing: a request is
// a system prompt, one user message and the JSON Schema the answer must
// follow; the answer is that JSON and what it cost in tokens.
package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Request is one question with a fixed shape of answer.
type Request struct {
	System    string         // stable instructions; cached by providers that can
	User      string         // the question with its evidence
	Schema    map[string]any // the answer's JSON Schema
	MaxTokens int64
}

// Usage is what a call consumed.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Response is the answer: JSON that follows the request's schema.
type Response struct {
	JSON    []byte
	Model   string // the model that answered, which a fallback may change
	Usage   Usage
	Latency time.Duration
}

// Provider answers requests.
type Provider interface {
	Name() string
	Model() string
	Complete(ctx context.Context, r Request) (Response, error)
}

// ErrRefused is a request the model declined to answer.
var ErrRefused = errors.New("the model declined to answer")

// Anthropic calls Claude through the Messages API.
type Anthropic struct {
	model, effort, keyFile, baseURL string
}

func NewAnthropic(model, effort, keyFile, baseURL string) *Anthropic {
	return &Anthropic{model: model, effort: effort, keyFile: keyFile, baseURL: baseURL}
}

func (a *Anthropic) Name() string  { return "anthropic" }
func (a *Anthropic) Model() string { return a.model }

// fallbackModels accept the server-side refusal fallback ("default" form):
// a request one model declines for policy reasons is answered by another
// inside the same call.
var fallbackModels = []string{"claude-opus-5", "claude-fable-5-1", "claude-sonnet-5-5"}

func (a *Anthropic) Complete(ctx context.Context, r Request) (Response, error) {
	key, err := os.ReadFile(a.keyFile)
	if err != nil {
		return Response{}, fmt.Errorf("reading the API key: %w", err)
	}
	// Only vigil's own settings count: an ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL
	// or credential profile in the server's environment is not used.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(strings.TrimSpace(string(key))),
		option.WithMaxRetries(2), option.WithRequestTimeout(2 * time.Minute)}
	if a.baseURL != "" {
		opts = append(opts, option.WithBaseURL(a.baseURL))
	}
	client := anthropic.NewClient(opts...)
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: r.MaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: r.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(r.User))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(a.effort),
			Format: anthropic.BetaJSONOutputFormatParam{Schema: r.Schema},
		},
	}
	for _, m := range fallbackModels {
		if strings.HasPrefix(a.model, m) {
			params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
			params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		}
	}
	start := time.Now()
	msg, err := client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return Response{}, fmt.Errorf("the provider refused the request (HTTP %d): %s", apiErr.StatusCode, short(apiErr.Error()))
		}
		return Response{}, fmt.Errorf("the provider could not be reached: %w", err)
	}
	res := Response{Model: string(msg.Model), Latency: time.Since(start), Usage: Usage{Input: msg.Usage.InputTokens,
		Output: msg.Usage.OutputTokens, CacheRead: msg.Usage.CacheReadInputTokens, CacheWrite: msg.Usage.CacheCreationInputTokens}}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return res, ErrRefused
	case anthropic.BetaStopReasonMaxTokens:
		return res, fmt.Errorf("the answer was cut off at %d tokens", r.MaxTokens)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	res.JSON = []byte(text.String())
	return res, nil
}

func short(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
