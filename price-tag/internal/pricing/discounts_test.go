package pricing

import (
	"math"
	"testing"
)

func TestDefaultDiscountsApplyOnlyToApprovedFamilies(t *testing.T) {
	prices := []ModelPrice{
		{Model: "gpt-5.6", Provider: "openai", InputCost: 10, OutputCost: 20, CacheReadCost: 3, CacheWriteCost: 4},
		{Model: "gemini-3.6-flash", Provider: "vertex", InputCost: 10, OutputCost: 20, CacheReadCost: 3, CacheWriteCost: 4},
		{Model: "claude-opus-5-5", Provider: "anthropic", InputCost: 10, OutputCost: 20, CacheReadCost: 3, CacheWriteCost: 4},
		{Model: "rits/zai-org/glm-5-3", Provider: "vllm", InputCost: 0, OutputCost: 0, CacheReadCost: 0, CacheWriteCost: 0},
	}
	got, changed, err := ApplyDiscounts(prices, DefaultDiscountPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed=%v, want two discounted models", changed)
	}
	if !closeDiscount(got[0].InputCost, 8) || !closeDiscount(got[0].OutputCost, 16) || !closeDiscount(got[0].CacheReadCost, 2.4) || !closeDiscount(got[0].CacheWriteCost, 3.2) {
		t.Fatalf("OpenAI rates=%+v", got[0])
	}
	if !closeDiscount(got[1].InputCost, 7.8) || !closeDiscount(got[1].OutputCost, 15.6) || !closeDiscount(got[1].CacheReadCost, 2.34) || !closeDiscount(got[1].CacheWriteCost, 3.12) {
		t.Fatalf("Gemini rates=%+v", got[1])
	}
	if got[2] != prices[2] || got[3] != prices[3] {
		t.Fatalf("Claude/GLM should remain unchanged: got=%+v", got)
	}
}

func closeDiscount(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestApplyDiscountsMatchesVertexProviderAliases(t *testing.T) {
	prices := []ModelPrice{{Model: "gemini-3.7-flash", Provider: "vertex_ai-language-models", InputCost: 1, OutputCost: 2, CacheReadCost: 0.2, CacheWriteCost: 1}}
	got, changed, err := ApplyDiscounts(prices, DefaultDiscountPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || !closeDiscount(got[0].InputCost, .78) || !closeDiscount(got[0].OutputCost, 1.56) || !closeDiscount(got[0].CacheReadCost, .156) || !closeDiscount(got[0].CacheWriteCost, .78) {
		t.Fatalf("vertex alias was not discounted across all rates: got=%+v changed=%v", got[0], changed)
	}
}

func TestDiscountPolicyRejectsOverlappingRules(t *testing.T) {
	err := ValidateDiscountPolicy(DiscountPolicy{
		Version: "test",
		Rules: []DiscountRule{
			{Name: "all-openai", Provider: "openai", Multiplier: .8},
			{Name: "gpt-only", Provider: "openai", ModelPrefix: "gpt-", Multiplier: .7},
		},
	})
	if err == nil {
		t.Fatal("expected overlapping rules to be rejected")
	}
}

func TestDiscountPolicyRejectsWildcardProviderOverlap(t *testing.T) {
	err := ValidateDiscountPolicy(DiscountPolicy{
		Version: "test",
		Rules: []DiscountRule{
			{Name: "all-gemini", ModelPrefix: "gemini-", Multiplier: .8},
			{Name: "vertex-gemini", Provider: "vertex", ModelPrefix: "gemini-", Multiplier: .78},
		},
	})
	if err == nil {
		t.Fatal("expected wildcard-provider overlap to be rejected")
	}
}
