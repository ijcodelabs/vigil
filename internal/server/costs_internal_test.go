package server

import (
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/fleet"
	"github.com/ijcodelabs/vigil/internal/rules"
	"github.com/prometheus/common/model"
)

func TestPriceMatching(t *testing.T) {
	tokens := []config.TokenPrice{{Model: "claude-sonnet", Input: 3, Output: 15}, {Model: "claude-sonnet-4-5", Input: 4, Output: 20},
		{Model: "gpt-4o", Input: 2.5, Output: 10}, {Model: "GPT-4o-mini", Input: 0.15, Output: 0.6}}
	for name, want := range map[string]float64{
		"claude-sonnet-4-20250514": 3, "claude-sonnet-4-5-20250929": 4, "gpt-4o-2024-08-06": 2.5, "gpt-4o-mini-2024-07-18": 0.15, "Claude-Sonnet-X": 3,
	} {
		if p, ok := tokenPrice(tokens, name); !ok || p.Input != want {
			t.Errorf("%s: input price %v (found %v), want %v", name, p.Input, ok, want)
		}
	}
	if _, ok := tokenPrice(tokens, "llama-3.1-70b"); ok {
		t.Error("a model that is not on the list got a price")
	}
	gpus := []config.GPUPrice{{Model: "", Price: 1}, {Model: "NVIDIA A100", Price: 3}, {Model: "NVIDIA A100-SXM4-80GB", Price: 3.5}}
	for name, want := range map[string]float64{"NVIDIA A100-SXM4-80GB": 3.5, "NVIDIA A100-PCIE-40GB": 3, "NVIDIA GeForce RTX 3090": 1, "AMD 1002:744C": 1} {
		if p, ok := gpuPrice(gpus, name); !ok || p != want {
			t.Errorf("%s: %v (found %v), want %v", name, p, ok, want)
		}
	}
	if _, ok := gpuPrice(gpus[1:], "AMD 1002:744C"); ok {
		t.Error("without a default entry an unknown card should have no price")
	}
}

// The server's own share of the cost metric: model calls of applications,
// one sample a minute like every other source.
func TestCostRuleGroups(t *testing.T) {
	groups := costRuleGroups()
	if len(groups) != 1 || groups[0].Name != "ai_costs" || groups[0].Interval != model.Duration(time.Minute) || len(groups[0].Rules) != 2 {
		t.Fatalf("groups: %+v", groups)
	}
	r := groups[0].Rules[0]
	if r.Record != "ai_cost_usd_per_minute" || r.Labels["source"] != "application" || r.Labels["billing"] != "api" || !strings.Contains(r.Expr, "genai_token_price") {
		t.Errorf("rule: %+v", r)
	}
	if own := groups[0].Rules[1]; own.Labels["source"] != "vigil" || !strings.Contains(own.Expr, "vigil_ai_cost_total") {
		t.Errorf("vigil's own: %+v", own)
	}
}

// Every group that writes ai_cost_usd_per_minute counts its minutes once:
// the server's own and those of the integrations. A group that would not
// be aligned (an alert added to it, another interval) fails here.
func TestCostGroupsAligned(t *testing.T) {
	groups := costRuleGroups()
	cat, err := fleet.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cat.Names() {
		gs, err := cat[name].RuleGroups(map[string]string{})
		if err != nil {
			continue // needs parameters this test does not give; none of those record costs
		}
		groups = append(groups, gs...)
	}
	found := 0
	for i := range groups {
		g := &groups[i]
		costs := false
		for _, r := range g.Rules {
			costs = costs || r.Record == "ai_cost_usd_per_minute"
		}
		alignPerMinute(g)
		if costs {
			found++
			if g.Delay != rules.AlignDelay {
				t.Errorf("group %s records AI costs but is not aligned", g.Name)
			}
		}
	}
	if found < 3 {
		t.Errorf("found %d groups recording AI costs, want the server's, gpu's and ai_module_claude's", found)
	}

	// Not aligned: a group with an alert, another interval, another kind of metric.
	for _, g := range []rules.Group{
		{Name: "a", Interval: model.Duration(time.Minute), Rules: []rules.Rule{{Record: "x_per_minute", Expr: "1"}, {Alert: "A", Expr: "1"}}},
		{Name: "b", Interval: model.Duration(30 * time.Second), Rules: []rules.Rule{{Record: "x_per_minute", Expr: "1"}}},
		{Name: "c", Interval: model.Duration(time.Minute), Rules: []rules.Rule{{Record: "x_total", Expr: "1"}}},
	} {
		alignPerMinute(&g)
		if g.Delay != 0 {
			t.Errorf("group %s was aligned", g.Name)
		}
	}
}
