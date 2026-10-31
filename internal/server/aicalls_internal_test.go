package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/ai"
	"github.com/ijcodelabs/vigil/internal/config"
)

// fakeAI answers every request the same way.
type fakeAI struct {
	res ai.Response
	err error
}

func (f fakeAI) Name() string                                              { return "fake" }
func (f fakeAI) Model() string                                             { return "claude-opus-5-5" }
func (f fakeAI) Complete(context.Context, ai.Request) (ai.Response, error) { return f.res, f.err }
func (f fakeAI) Verify(context.Context) error                              { return nil }

// Explanations made before the ledger existed get their calls once, so
// this month's spend and the cost metric's totals stay what they were.
func TestAICallsBackfill(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The explanations table as it was, with two explanations in it.
	if _, err := st.db.Exec(`CREATE TABLE explanations (id INTEGER PRIMARY KEY AUTOINCREMENT, incident INTEGER NOT NULL,
		created INTEGER NOT NULL, user TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, prompt TEXT NOT NULL,
		bundle_hash TEXT NOT NULL, evidence TEXT NOT NULL, answer TEXT NOT NULL, error TEXT NOT NULL, usage TEXT NOT NULL,
		cost REAL NOT NULL, latency_ms INTEGER NOT NULL, verdict TEXT NOT NULL DEFAULT '', verdict_by TEXT NOT NULL DEFAULT '',
		verdict_note TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, row := range []struct {
		incident int64
		subject  string
		cost     float64
	}{{7, "", 0.25}, {0, "log:abc", 0.5}} {
		if _, err := st.db.Exec(`INSERT INTO explanations (incident, subject, created, user, provider, model, prompt, bundle_hash,
			evidence, answer, error, usage, cost, latency_ms) VALUES (?, ?, ?, 'admin', 'anthropic', 'claude-opus-5-5', 'explain-1', 'h',
			'[]', 'null', '', '{}', ?, 10)`, row.incident, row.subject, now, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 { // the second start finds nothing to do
		if err := initExplanations(st.db); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st, cfg: &config.Server{}}
	if spent, err := s.aiSpentSince(context.Background(), monthStart(time.Now())); err != nil || spent != 0.75 {
		t.Errorf("spent after the backfill: %v %v", spent, err)
	}
	var subjects, unlinked int
	st.db.QueryRow(`SELECT COUNT(*) FROM ai_calls WHERE subject IN ('incident:7', 'log:abc') AND tool = 'explain'`).Scan(&subjects)
	st.db.QueryRow(`SELECT COUNT(*) FROM explanations WHERE call = 0`).Scan(&unlinked)
	if subjects != 2 || unlinked != 0 {
		t.Errorf("calls with their subjects: %d, explanations without a call: %d", subjects, unlinked)
	}
}

// A failed call is recorded and counts against the budget: it can cost
// tokens too. Explain keeps its series as it was; other tools get a label.
func TestAICallsLedger(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := initExplanations(st.db); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Server{}
	cfg.AI.Model = "claude-opus-5-5"
	cfg.Costs.Tokens = []config.TokenPrice{{Model: "claude-opus-5-5", Input: 4, Output: 20}}
	s := &Server{store: st, cfg: cfg, ai: fakeAI{res: ai.Response{Usage: ai.Usage{Input: 100000}}, err: errors.New("overloaded")}}
	ctx := context.Background()

	c, _, err := s.callAI(ctx, aiCall{User: "admin", Tool: "explain", Subject: "incident:1", Prompt: "explain-1"}, ai.Request{})
	if err != nil || c.ID == 0 || c.Error != "overloaded" || c.Cost != 0.4 {
		t.Fatalf("failed call: %+v %v", c, err)
	}
	if st, err := s.aiState(ctx); err != nil || st.Spent != 0.4 {
		t.Errorf("spent: %+v %v", st, err)
	}

	st.db.Exec(`INSERT INTO ai_calls (created, user, tool, subject, provider, model, prompt, usage, cost, latency_ms, error)
		VALUES (?, 'admin', 'view', 'draft:1', 'fake', 'claude-opus-5-5', 'view-1', '{}', 0.1, 1, '')`, time.Now().Unix())
	s.cfg.Costs.Tokens = nil // priceSamples would query the metric store for token prices
	samples, err := s.priceSamples(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, sm := range samples {
		if sm.Name == "vigil_ai_cost_total" {
			got[sm.Labels["tool"]+"/"+sm.Labels["model"]] = sm.Value
		}
	}
	if len(got) != 2 || got["/claude-opus-5-5"] != 0.4 || got["view/claude-opus-5-5"] != 0.1 {
		t.Errorf("cost series: %v", got)
	}
}
