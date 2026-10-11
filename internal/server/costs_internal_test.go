package server

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/config"
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
