package pricing

import (
	"fmt"
	"strings"
)

// DiscountRule applies a multiplier to all four effective gateway rates for
// matching models. List prices are deliberately never transformed.
type DiscountRule struct {
	Name        string  `json:"name"`
	Provider    string  `json:"provider,omitempty"`
	ModelPrefix string  `json:"model_prefix,omitempty"`
	Multiplier  float64 `json:"multiplier"`
}

type DiscountPolicy struct {
	Version string         `json:"version"`
	Rules   []DiscountRule `json:"rules"`
}

// DefaultDiscountPolicy is the approved initial commercial policy. GLM and
// Claude intentionally have no rule: their existing explicit rates remain
// unchanged.
func DefaultDiscountPolicy() DiscountPolicy {
	return DiscountPolicy{
		Version: "ge-commercial-v1",
		Rules: []DiscountRule{
			{Name: "openai-ge-api-key", Provider: "openai", Multiplier: 0.80},
			{Name: "gemini-ge-cloud", Provider: "vertex", ModelPrefix: "gemini-", Multiplier: 0.78},
		},
	}
}

func ValidateDiscountPolicy(policy DiscountPolicy) error {
	if strings.TrimSpace(policy.Version) == "" {
		return fmt.Errorf("policy version is required")
	}
	if len(policy.Rules) > 100 {
		return fmt.Errorf("at most 100 discount rules are allowed")
	}
	seen := make(map[string]struct{}, len(policy.Rules))
	for i, rule := range policy.Rules {
		if strings.TrimSpace(rule.Name) == "" {
			return fmt.Errorf("rule %d: name is required", i)
		}
		if _, ok := seen[rule.Name]; ok {
			return fmt.Errorf("duplicate rule name %q", rule.Name)
		}
		seen[rule.Name] = struct{}{}
		if rule.Multiplier <= 0 || rule.Multiplier > 10 {
			return fmt.Errorf("rule %q: multiplier must be greater than 0 and at most 10", rule.Name)
		}
		if strings.TrimSpace(rule.Provider) == "" && strings.TrimSpace(rule.ModelPrefix) == "" {
			return fmt.Errorf("rule %q: provider or model_prefix is required", rule.Name)
		}
	}
	for i, left := range policy.Rules {
		for _, right := range policy.Rules[i+1:] {
			if providersOverlap(left.Provider, right.Provider) && prefixesOverlap(left.ModelPrefix, right.ModelPrefix) {
				return fmt.Errorf("rules %q and %q overlap; use one deterministic rule", left.Name, right.Name)
			}
		}
	}
	return nil
}

func providersOverlap(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return a == "" || b == "" || a == b
}

func prefixesOverlap(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return a == "" || b == "" || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// ApplyDiscounts transforms effective rates only. It returns the changed model
// IDs for safe provenance logging.
func ApplyDiscounts(prices []ModelPrice, policy DiscountPolicy) ([]ModelPrice, []string, error) {
	if err := ValidateDiscountPolicy(policy); err != nil {
		return nil, nil, err
	}
	result := make([]ModelPrice, len(prices))
	copy(result, prices)
	changed := make([]string, 0)
	for i := range result {
		for _, rule := range policy.Rules {
			if !matchesDiscount(result[i], rule) {
				continue
			}
			result[i].InputCost *= rule.Multiplier
			result[i].OutputCost *= rule.Multiplier
			result[i].CacheReadCost *= rule.Multiplier
			result[i].CacheWriteCost *= rule.Multiplier
			changed = append(changed, result[i].Model)
			break
		}
	}
	return result, changed, nil
}

// DiscountPercent reports the policy discount for a model without changing
// any rates. It is used by presentation layers when a vendor-list baseline is
// intentionally unavailable (for example, Vertex reseller pricing).
func DiscountPercent(price ModelPrice, policy DiscountPolicy) (float64, bool) {
	for _, rule := range policy.Rules {
		if matchesDiscount(price, rule) {
			return (1 - rule.Multiplier) * 100, true
		}
	}
	return 0, false
}

func matchesDiscount(price ModelPrice, rule DiscountRule) bool {
	provider := normalizeDiscountProvider(rule.Provider)
	model := strings.ToLower(price.Model)
	return (provider == "" || normalizeDiscountProvider(price.Provider) == provider) &&
		(rule.ModelPrefix == "" || strings.HasPrefix(model, strings.ToLower(rule.ModelPrefix)))
}

// normalizeDiscountProvider keeps policy matching stable across provider names
// emitted by LiteLLM, the gateway, and older seeded rows. Vertex rows have
// appeared as vertex, vertex_ai, and vertex_ai-language-models.
func normalizeDiscountProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case p == "", p == "*":
		return ""
	case p == "openai" || strings.HasPrefix(p, "openai_") || strings.HasPrefix(p, "openai-") || strings.HasPrefix(p, "openai/"):
		return "openai"
	case p == "google" || p == "gemini" || strings.HasPrefix(p, "vertex") || strings.HasPrefix(p, "google-") || strings.HasPrefix(p, "google_"):
		return "vertex"
	default:
		return p
	}
}
