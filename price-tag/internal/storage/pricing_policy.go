package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/redhat-et/pricetag-metering/internal/pricing"
)

func (s *Store) GetPricingDiscountPolicy(ctx context.Context) (pricing.DiscountPolicy, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT rules FROM pricing_discount_policy WHERE id = true`).Scan(&raw)
	if err == sql.ErrNoRows || len(raw) == 0 {
		return pricing.DefaultDiscountPolicy(), nil
	}
	if err != nil {
		return pricing.DiscountPolicy{}, err
	}
	var policy pricing.DiscountPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return pricing.DiscountPolicy{}, fmt.Errorf("decode discount policy: %w", err)
	}
	if err := pricing.ValidateDiscountPolicy(policy); err != nil {
		return pricing.DiscountPolicy{}, err
	}
	return policy, nil
}

func (s *Store) UpdatePricingDiscountPolicy(ctx context.Context, actor string, policy pricing.DiscountPolicy) error {
	if err := pricing.ValidateDiscountPolicy(policy); err != nil {
		return err
	}
	rules, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pricing_discount_policy (id, version, rules, updated_by)
		VALUES (true, $1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET version=EXCLUDED.version, rules=EXCLUDED.rules,
		updated_by=EXCLUDED.updated_by, updated_at=NOW()`, policy.Version, rules, actor); err != nil {
		return err
	}
	if err := s.auditTx(ctx, tx, actor, "pricing.discount_policy_update", "pricing", map[string]any{
		"version": policy.Version, "rules": policy.Rules,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
