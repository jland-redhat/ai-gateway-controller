package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func qosTestConfig() QosConfig {
	return QosConfig{
		Enabled:             true,
		WindowSeconds:       60,
		HoldWindowSeconds:   300,
		PremiumMaxTokens:    600_000,
		RatelimitMinTokens:  2_000_000,
		HoldMainTokens:      3_000_000,
		HoldRatelimitTokens: 10_000_000,
		CacheReadWeight:     0.05,
		ClassCacheTTL:       time.Minute,
		HoldCacheTTL:        time.Minute,
		Providers:           []string{"cb-glm"},
		ExemptTagKey:        "origin",
		ExemptTagValue:      "loadtest",
	}
}

func TestClassifyQos(t *testing.T) {
	cfg := qosTestConfig()
	cases := []struct {
		name         string
		window, hold int64
		want         string
	}{
		{"premium", 100_000, 500_000, QosPremium},
		{"window main boundary", 600_000, 600_000, QosMain},
		{"window ratelimit boundary", 2_000_000, 2_000_000, QosRatelimit},
		{"hold main", 100_000, 3_000_000, QosMain},
		{"hold ratelimit", 100_000, 10_000_000, QosRatelimit},
		{"hold ratelimit beats main", 700_000, 10_000_000, QosRatelimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyQos(tc.window, tc.hold, cfg); got != tc.want {
				t.Fatalf("classifyQos(%d, %d) = %q, want %q", tc.window, tc.hold, got, tc.want)
			}
		})
	}
}

func TestGetQosStatusBoundariesAndHoldGuard(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "11111111-1111-1111-1111-111111111111"
	email := "qos-user@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "User",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}

	insertQosUsage(t, store, email, time.Now().Add(-20*time.Second), 100_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosPremium)

	insertQosUsage(t, store, email, time.Now().Add(-10*time.Second), 500_000, 0, 0, 0)
	store.SetQosConfig(qosTestConfig()) // reset caches for the boundary case
	assertQosClass(t, store, ctx, email, userID, QosMain)

	insertQosUsage(t, store, email, time.Now().Add(-5*time.Second), 1_500_000, 0, 0, 0)
	store.SetQosConfig(qosTestConfig())
	assertQosClass(t, store, ctx, email, userID, QosRatelimit)
}

func TestGetQosStatusHoldGuard(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "22222222-2222-2222-2222-222222222222"
	email := "qos-hold@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Hold",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}

	// Keep the 60s window under the premium boundary but push the 5m guard
	// over the main boundary.
	insertQosUsage(t, store, email, time.Now().Add(-2*time.Minute), 1_000_000, 0, 0, 0)
	insertQosUsage(t, store, email, time.Now().Add(-3*time.Minute), 1_000_000, 0, 0, 0)
	insertQosUsage(t, store, email, time.Now().Add(-4*time.Minute), 1_000_000, 0, 0, 0)
	insertQosUsage(t, store, email, time.Now().Add(-10*time.Second), 100_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosMain)

	// Push the guard over the ratelimit boundary without moving the 60s window.
	insertQosUsage(t, store, email, time.Now().Add(-2*time.Minute-10*time.Second), 7_000_000, 0, 0, 0)
	store.SetQosConfig(qosTestConfig())
	assertQosClass(t, store, ctx, email, userID, QosRatelimit)
}

func TestGetQosStatusProviderScope(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "33333333-3333-3333-3333-333333333333"
	email := "qos-scope@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Scope",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}

	// Paid-provider usage does not count toward the cb-glm class.
	insertQosUsageProvider(t, store, email, "openai", time.Now().Add(-10*time.Second), 3_000_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosPremium)
}

func TestGetQosStatusExemption(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "55555555-5555-5555-5555-555555555555"
	email := "qos-exempt@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Exempt", "origin": "loadtest",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}

	// Heavy cb-glm usage is still premium for a tagged load-test user.
	insertQosUsage(t, store, email, time.Now().Add(-10*time.Second), 3_000_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosPremium)
}

func TestGetQosStatusMultiLoginAggregation(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "44444444-4444-4444-4444-444444444444"
	email := "qos-primary@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Multi",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}
	second := "qos-secondary@example.com"
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO partner_user_logins (username,user_id,is_current) VALUES ($1,$2,FALSE)`,
		second, userID); err != nil {
		t.Fatalf("insert second login: %v", err)
	}
	insertQosUsage(t, store, email, time.Now().Add(-10*time.Second), 400_000, 0, 0, 0)
	insertQosUsage(t, store, second, time.Now().Add(-5*time.Second), 400_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosMain)
}

func TestGetQosStatusCacheTTL(t *testing.T) {
	store, ctx := openTestStore(t)
	cfg := qosTestConfig()
	cfg.ClassCacheTTL = 20 * time.Millisecond
	cfg.HoldCacheTTL = time.Minute
	store.SetQosConfig(cfg)

	userID := "55555555-5555-5555-5555-555555555555"
	email := "qos-cache@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Cache",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}
	insertQosUsage(t, store, email, time.Now().Add(-10*time.Second), 100_000, 0, 0, 0)
	assertQosClass(t, store, ctx, email, userID, QosPremium)

	// Within the short cache TTL the old class is served even though a
	// main-level event now exists.
	insertQosUsage(t, store, email, time.Now().Add(-5*time.Second), 600_000, 0, 0, 0)
	decision, _, err := store.QuotaDecisionCached(ctx, email, false)
	if err != nil {
		t.Fatalf("QuotaDecisionCached: %v", err)
	}
	status, err := store.GetQosStatus(ctx, email, decision, false)
	if err != nil {
		t.Fatalf("GetQosStatus cached: %v", err)
	}
	if status.Class != QosPremium {
		t.Fatalf("cache TTL not honored: got %q, want premium", status.Class)
	}

	time.Sleep(30 * time.Millisecond)
	status, err = store.GetQosStatus(ctx, email, decision, false)
	if err != nil {
		t.Fatalf("GetQosStatus after TTL: %v", err)
	}
	if status.Class != QosMain {
		t.Fatalf("cache did not refresh: got %q, want main", status.Class)
	}
}

func TestGetQosStatusUnmatchedDefaultsMain(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	status, err := store.GetQosStatus(ctx, "unknown-user", QuotaDecision{Logins: []string{"unknown-user"}}, false)
	if err != nil {
		t.Fatalf("GetQosStatus: %v", err)
	}
	if status.Class != QosMain {
		t.Fatalf("unmatched class = %q, want main", status.Class)
	}
	if status.Subject != "" {
		t.Fatalf("unmatched subject = %q, want empty", status.Subject)
	}
}

func TestGetMonthlyUsageIncludesQosFields(t *testing.T) {
	store, ctx := openTestStore(t)
	store.SetQosConfig(qosTestConfig())

	userID := "66666666-6666-6666-6666-666666666666"
	email := "qos-response@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Response",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}
	insertQosUsage(t, store, email, time.Now().Add(-5*time.Second), 700_000, 0, 0, 0)
	stats, err := store.GetMonthlyUsage(ctx, email, "rits/zai-org/glm-5-3", false)
	if err != nil {
		t.Fatalf("GetMonthlyUsage: %v", err)
	}
	if stats.QosClass != QosMain {
		t.Fatalf("QosClass = %q, want main", stats.QosClass)
	}
	if stats.QosSubject != userID {
		t.Fatalf("QosSubject = %q, want %q", stats.QosSubject, userID)
	}
	if stats.QosWindowTokens < 700_000 {
		t.Fatalf("QosWindowTokens = %d, want >= 700000", stats.QosWindowTokens)
	}
}

func assertQosClass(t *testing.T, store *Store, ctx context.Context, username, userID, want string) {
	t.Helper()
	decision, _, err := store.QuotaDecisionCached(ctx, username, false)
	if err != nil {
		t.Fatalf("QuotaDecisionCached: %v", err)
	}
	if decision.Subject != userID {
		t.Fatalf("Subject = %q, want %q", decision.Subject, userID)
	}
	status, err := store.GetQosStatus(ctx, username, decision, false)
	if err != nil {
		t.Fatalf("GetQosStatus: %v", err)
	}
	if status.Class != want {
		t.Fatalf("class = %q, want %q (window=%d hold=%d)", status.Class, want, status.WindowTokens, status.HoldWindowTokens)
	}
}

func insertQosUsage(t *testing.T, store *Store, username string, ts time.Time, prompt, cached, cacheWrite, completion int) {
	t.Helper()
	insertQosUsageProvider(t, store, username, "cb-glm", ts, prompt, cached, cacheWrite, completion)
}

func insertQosUsageProvider(t *testing.T, store *Store, username, provider string, ts time.Time, prompt, cached, cacheWrite, completion int) {
	t.Helper()
	insertQosUsageProviderModel(t, store, username, provider, "rits/zai-org/glm-5-3", ts, prompt, cached, cacheWrite, completion)
}

func insertQosUsageProviderModel(t *testing.T, store *Store, username, provider, model string, ts time.Time, prompt, cached, cacheWrite, completion int) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `
		INSERT INTO usage_events
			(event_id, timestamp, username, provider, model,
			 prompt_tokens, completion_tokens, total_tokens,
			 cached_input_tokens, cache_creation_tokens, reasoning_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0)`,
		fmt.Sprintf("qos-test-%d-%d", time.Now().UnixNano(), prompt),
		ts, username, provider, model,
		prompt, completion, prompt+completion, cached, cacheWrite)
	if err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
}

func TestSetQosConfigThresholdValidation(t *testing.T) {
	var store Store
	store.SetQosConfig(QosConfig{Enabled: true})
	store.qosMu.Lock()
	cfg := store.qosConfig
	store.qosMu.Unlock()
	if cfg.PremiumMaxTokens != qosDefaultPremiumMaxTokens ||
		cfg.RatelimitMinTokens != qosDefaultRatelimitMinTokens ||
		cfg.HoldMainTokens != qosDefaultHoldMainTokens ||
		cfg.HoldRatelimitTokens != qosDefaultHoldRatelimitTokens {
		t.Fatalf("zero thresholds were not defaulted: %+v", cfg)
	}

	store.SetQosConfig(QosConfig{
		Enabled:             true,
		PremiumMaxTokens:    5_000_000,
		RatelimitMinTokens:  1_000_000,
		HoldMainTokens:      9_000_000,
		HoldRatelimitTokens: 2_000_000,
	})
	store.qosMu.Lock()
	cfg = store.qosConfig
	store.qosMu.Unlock()
	if cfg.PremiumMaxTokens != qosDefaultPremiumMaxTokens || cfg.RatelimitMinTokens != qosDefaultRatelimitMinTokens {
		t.Fatalf("inverted window thresholds were not defaulted: %+v", cfg)
	}
	if cfg.HoldMainTokens != qosDefaultHoldMainTokens || cfg.HoldRatelimitTokens != qosDefaultHoldRatelimitTokens {
		t.Fatalf("inverted hold thresholds were not defaulted: %+v", cfg)
	}
}

func insertQosUsageReasoning(t *testing.T, store *Store, username string, ts time.Time, prompt, cached, cacheWrite, completion, reasoning int) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `
		INSERT INTO usage_events
			(event_id, timestamp, username, provider, model,
			 prompt_tokens, completion_tokens, total_tokens,
			 cached_input_tokens, cache_creation_tokens, reasoning_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		fmt.Sprintf("qos-test-%d-%d", time.Now().UnixNano(), prompt),
		ts, username, "cb-glm", "rits/zai-org/glm-5-3",
		prompt, completion, prompt+completion, cached, cacheWrite, reasoning)
	if err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
}

// TestQosReasoningCountedOnce pins that reasoning tokens are already part of
// completion_tokens. The 1,000 completion tokens (800 of them reasoning) sit
// below the 1,500 premium boundary; adding reasoning separately would count
// 1,800 and misclassify the caller as main.
func TestQosReasoningCountedOnce(t *testing.T) {
	store, ctx := openTestStore(t)
	cfg := qosTestConfig()
	cfg.PremiumMaxTokens = 1_500
	cfg.RatelimitMinTokens = 3_000
	cfg.HoldMainTokens = 10_000
	cfg.HoldRatelimitTokens = 20_000
	store.SetQosConfig(cfg)

	userID := "44444444-4444-4444-4444-444444444444"
	email := "qos-reasoning@example.com"
	if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
		"email": email, "first_name": "QoS", "last_name": "Reasoning",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}

	insertQosUsageReasoning(t, store, email, time.Now().Add(-10*time.Second), 0, 0, 0, 1_000, 800)
	assertQosClass(t, store, ctx, email, userID, QosPremium)
}

func TestSetQosConfigAllowsZeroCacheReadWeight(t *testing.T) {
	var store Store
	store.SetQosConfig(QosConfig{Enabled: true, CacheReadWeight: 0})
	store.qosMu.Lock()
	cfg := store.qosConfig
	store.qosMu.Unlock()
	if cfg.CacheReadWeight != 0 {
		t.Fatalf("cache read weight = %v, want 0 (cache reads are free)", cfg.CacheReadWeight)
	}

	store.SetQosConfig(QosConfig{Enabled: true, CacheReadWeight: -1})
	store.qosMu.Lock()
	cfg = store.qosConfig
	store.qosMu.Unlock()
	if cfg.CacheReadWeight != 0.05 {
		t.Fatalf("negative cache read weight = %v, want default 0.05", cfg.CacheReadWeight)
	}
}

func TestQosKeyAliases(t *testing.T) {
	cfg := qosTestConfig()
	cfg.KeyAliasPremium = "premium-key"
	cfg.KeyAliasMain = "main-key"
	cfg.KeyAliasRatelimit = "ratelimit-key"
	if got := qosKeyAlias(QosPremium, cfg); got != "premium-key" {
		t.Fatalf("premium alias = %q", got)
	}
	if got := qosKeyAlias(QosMain, cfg); got != "main-key" {
		t.Fatalf("main alias = %q", got)
	}
	if got := qosKeyAlias(QosRatelimit, cfg); got != "ratelimit-key" {
		t.Fatalf("ratelimit alias = %q", got)
	}
	if got := qosKeyAlias("unknown", cfg); got != "main-key" {
		t.Fatalf("unknown class alias = %q, want main-key", got)
	}
}

func TestSetQosConfigKeyAliasDefaults(t *testing.T) {
	var store Store
	store.SetQosConfig(QosConfig{Enabled: true})
	store.qosMu.Lock()
	cfg := store.qosConfig
	store.qosMu.Unlock()
	if cfg.KeyAliasPremium != "premium" || cfg.KeyAliasMain != "main" || cfg.KeyAliasRatelimit != "main" {
		t.Fatalf("default aliases = %q/%q/%q, want premium/main/main",
			cfg.KeyAliasPremium, cfg.KeyAliasMain, cfg.KeyAliasRatelimit)
	}
}

func TestQuotaRetryAfter(t *testing.T) {
	if got := quotaRetryAfter(""); got != 3600 {
		t.Fatalf("empty month end retry = %d, want 3600", got)
	}
	if got := quotaRetryAfter("not-a-date"); got != 3600 {
		t.Fatalf("invalid month end retry = %d, want 3600", got)
	}
	end := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	if got := quotaRetryAfter(end); got < 7100 || got > 7300 {
		t.Fatalf("two-hour retry = %d, want ~7200", got)
	}
}

func usageStatsJSON(t *testing.T, stats UsageStats) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal usage stats: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("unmarshal usage stats: %v", err)
	}
	return body
}

func TestGetMonthlyUsageDecisionContract(t *testing.T) {
	t.Run("allowed and QoS disabled", func(t *testing.T) {
		store, ctx := openTestStore(t)
		stats, err := store.GetMonthlyUsage(ctx, "contract-allowed@example.com", "model-a", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage: %v", err)
		}
		if !stats.Allowed || stats.DenyReason != "" || stats.QuotaRetryAfter != 0 {
			t.Fatalf("allowed contract = %+v", stats)
		}
		body := usageStatsJSON(t, stats)
		if got := body["allowed"]; got != true {
			t.Fatalf("allowed JSON = %v, want true", got)
		}
		for _, key := range []string{"denyReason", "quotaRetryAfter", "qosKeyAlias"} {
			if _, ok := body[key]; ok {
				t.Fatalf("%s present on allowed response: %v", key, body)
			}
		}
	})

	t.Run("model denial", func(t *testing.T) {
		store, ctx := openTestStore(t)
		userID := "77777777-7777-7777-7777-777777777777"
		email := "contract-model@example.com"
		if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
			"email": email, "first_name": "Contract", "last_name": "QoS",
		}); err != nil {
			t.Fatalf("CreatePartnerUser: %v", err)
		}
		if _, err := store.SetPartnerUserModelAllowlist(ctx, "test", userID, []string{}); err != nil {
			t.Fatalf("SetPartnerUserModelAllowlist: %v", err)
		}
		stats, err := store.GetMonthlyUsage(ctx, email, "model-a", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage: %v", err)
		}
		if stats.Allowed || stats.DenyReason != "model_not_allowed" || stats.QuotaRetryAfter != 0 {
			t.Fatalf("model denial contract = %+v", stats)
		}
		body := usageStatsJSON(t, stats)
		if body["allowed"] != false || body["denyReason"] != "model_not_allowed" {
			t.Fatalf("model denial JSON = %v", body)
		}
		if _, ok := body["quotaRetryAfter"]; ok {
			t.Fatalf("model denial included quotaRetryAfter: %v", body)
		}
	})

	t.Run("quota denial", func(t *testing.T) {
		store, ctx := openTestStore(t)
		email := "contract-quota@example.com"
		limit := 0.01
		enforced := true
		if _, err := store.UpdateQuotaPolicy(ctx, "test", QuotaPolicyUpdate{DefaultMonthlyUSD: &limit, Enforced: &enforced}); err != nil {
			t.Fatalf("UpdateQuotaPolicy: %v", err)
		}
		insertQosUsageProviderModel(t, store, email, "openai", "gpt-4o-mini", time.Now().Add(-time.Second), 1_000_000, 0, 0, 0)
		stats, err := store.GetMonthlyUsage(ctx, email, "gpt-4o-mini", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage: %v", err)
		}
		if stats.Allowed || stats.DenyReason != "quota_exhausted" || stats.QuotaRetryAfter <= 0 {
			t.Fatalf("quota denial contract = %+v", stats)
		}
		body := usageStatsJSON(t, stats)
		if body["allowed"] != false || body["denyReason"] != "quota_exhausted" {
			t.Fatalf("quota denial JSON = %v", body)
		}
		retry, ok := body["quotaRetryAfter"].(float64)
		if !ok || retry <= 0 {
			t.Fatalf("quota denial quotaRetryAfter = %v", body["quotaRetryAfter"])
		}
	})

	t.Run("QoS enabled alias", func(t *testing.T) {
		store, ctx := openTestStore(t)
		cfg := qosTestConfig()
		cfg.KeyAliasPremium = "premium-key"
		cfg.KeyAliasMain = "main-key"
		cfg.KeyAliasRatelimit = "ratelimit-key"
		store.SetQosConfig(cfg)
		userID := "99999999-9999-9999-9999-999999999999"
		email := "contract-qos@example.com"
		if _, err := store.CreatePartnerUser(ctx, "test", userID, map[string]any{
			"email": email, "first_name": "Contract", "last_name": "Model",
		}); err != nil {
			t.Fatalf("CreatePartnerUser: %v", err)
		}
		insertQosUsage(t, store, email, time.Now().Add(-time.Second), 700_000, 0, 0, 0)
		stats, err := store.GetMonthlyUsage(ctx, email, "rits/zai-org/glm-5-3", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage: %v", err)
		}
		if stats.QosClass != QosMain || stats.QosKeyAlias != "main-key" {
			t.Fatalf("QoS contract = class %q alias %q", stats.QosClass, stats.QosKeyAlias)
		}
	})

	t.Run("QoS premium and ratelimit aliases", func(t *testing.T) {
		store, ctx := openTestStore(t)
		cfg := qosTestConfig()
		cfg.KeyAliasPremium = "premium-key"
		cfg.KeyAliasMain = "main-key"
		cfg.KeyAliasRatelimit = "ratelimit-key"
		store.SetQosConfig(cfg)

		premiumID := "88888888-8888-8888-8888-888888888888"
		premiumEmail := "contract-premium@example.com"
		if _, err := store.CreatePartnerUser(ctx, "test", premiumID, map[string]any{
			"email": premiumEmail, "first_name": "Contract", "last_name": "Premium",
		}); err != nil {
			t.Fatalf("CreatePartnerUser premium: %v", err)
		}
		insertQosUsage(t, store, premiumEmail, time.Now().Add(-time.Second), 50_000, 0, 0, 0)
		premium, err := store.GetMonthlyUsage(ctx, premiumEmail, "rits/zai-org/glm-5-3", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage premium: %v", err)
		}
		if premium.QosClass != QosPremium || premium.QosKeyAlias != "premium-key" {
			t.Fatalf("premium contract = class %q alias %q", premium.QosClass, premium.QosKeyAlias)
		}

		ratelimitID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		ratelimitEmail := "contract-ratelimit@example.com"
		if _, err := store.CreatePartnerUser(ctx, "test", ratelimitID, map[string]any{
			"email": ratelimitEmail, "first_name": "Contract", "last_name": "Ratelimit",
		}); err != nil {
			t.Fatalf("CreatePartnerUser ratelimit: %v", err)
		}
		insertQosUsage(t, store, ratelimitEmail, time.Now().Add(-time.Second), 2_100_000, 0, 0, 0)
		ratelimit, err := store.GetMonthlyUsage(ctx, ratelimitEmail, "rits/zai-org/glm-5-3", false)
		if err != nil {
			t.Fatalf("GetMonthlyUsage ratelimit: %v", err)
		}
		if ratelimit.QosClass != QosRatelimit || ratelimit.QosKeyAlias != "ratelimit-key" {
			t.Fatalf("ratelimit contract = class %q alias %q", ratelimit.QosClass, ratelimit.QosKeyAlias)
		}
	})
}
