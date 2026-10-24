package server

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/rules"
)

// costRules prices the model calls of applications into the common cost
// metric; see the file's comment.
//
//go:embed cost_rules.yaml
var costRules string

func costRuleGroups() []rules.Group {
	f, err := rules.Parse(costRules)
	if err != nil {
		panic(fmt.Sprintf("cost_rules.yaml: %v", err)) // a build mistake; the test catches it
	}
	return f.Groups
}

// Prices become series, so that cost is an ordinary query: usage times
// price. For every model that agents report tokens for, and every GPU model
// they report, the server writes what the price list says:
//
//	genai_token_price{model, type}   price of ONE token of that kind
//	gpu_hour_price{model}            price of one such GPU for an hour
//
// The model label is exactly the one on the usage series, so the two join
// without any name matching in the query. A model the list has no price
// for gets no series, and so drops out of cost queries; the AI costs
// dashboard lists those.

// tokenPrice finds the entry whose model is the longest prefix of name.
func tokenPrice(list []config.TokenPrice, name string) (config.TokenPrice, bool) {
	best, found := config.TokenPrice{}, false
	lower := strings.ToLower(name)
	for _, p := range list {
		if strings.HasPrefix(lower, strings.ToLower(p.Model)) && (!found || len(p.Model) > len(best.Model)) {
			best, found = p, true
		}
	}
	return best, found
}

func gpuPrice(list []config.GPUPrice, name string) (float64, bool) {
	best, found := config.GPUPrice{}, false
	lower := strings.ToLower(name)
	for _, p := range list {
		if strings.HasPrefix(lower, strings.ToLower(p.Model)) && (!found || len(p.Model) > len(best.Model)) {
			best, found = p, true
		}
	}
	return best.Price, found
}

// priceSamples builds the price series for the models in use right now.
func (s *Server) priceSamples(ctx context.Context, now time.Time) ([]model.Sample, error) {
	costs := s.cfg.Costs
	ts := now.UnixMilli()
	var out []model.Sample
	backend := ruleBackend{s}
	if len(costs.Tokens) > 0 {
		series, err := backend.Query(ctx, "", `group by (model) (genai_tokens_total)`, now)
		if err != nil {
			return nil, err
		}
		for _, sr := range series {
			name := sr.Labels["model"]
			p, ok := tokenPrice(costs.Tokens, name)
			if !ok {
				continue
			}
			for typ, perMillion := range map[string]float64{"input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_write": p.CacheWrite} {
				// A kind without a price is left out, so that it shows as
				// unpriced rather than as free. Input and output are always written.
				if perMillion > 0 || typ == "input" || typ == "output" {
					out = append(out, model.Sample{Name: "genai_token_price", Labels: map[string]string{"model": name, "type": typ}, Value: perMillion / 1e6, TS: ts})
				}
			}
		}
	}
	// What vigil's own explanations cost so far, for the cost metric.
	if s.ai != nil {
		spent, err := s.aiSpendTotal(ctx)
		if err != nil {
			return nil, err
		}
		for m, usd := range spent {
			out = append(out, model.Sample{Name: "vigil_ai_cost_total", Labels: map[string]string{"model": m}, Value: usd, TS: ts})
		}
	}
	if len(costs.GPUHours) > 0 {
		series, err := backend.Query(ctx, "", `group by (model) (gpu_info)`, now)
		if err != nil {
			return nil, err
		}
		for _, sr := range series {
			if price, ok := gpuPrice(costs.GPUHours, sr.Labels["model"]); ok {
				out = append(out, model.Sample{Name: "gpu_hour_price", Labels: map[string]string{"model": sr.Labels["model"]}, Value: price, TS: ts})
			}
		}
	}
	return out, nil
}

// priceLoop keeps the price series fresh.
func (s *Server) priceLoop(ctx context.Context) {
	if len(s.cfg.Costs.Tokens) == 0 && len(s.cfg.Costs.GPUHours) == 0 {
		return
	}
	failing := false
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		samples, err := s.priceSamples(ctx, time.Now())
		if err == nil && len(samples) > 0 {
			err = s.forward(ctx, "", promwire.Encode(samples))
		}
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			s.log.Warn("could not write the price series; costs will be missing until this works", "err", err)
			failing = true
		case err == nil:
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
