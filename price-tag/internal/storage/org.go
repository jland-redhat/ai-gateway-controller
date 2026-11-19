package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The legacy org directory (people, person_identities, invites, import
// batches) was removed: partner_users is the single identity source. Only the
// shared audit log and the slug normalizer survive here, because the audit log
// records every operator write (partner user and quota mutations alike).

var orgMigrations = []string{
	`CREATE TABLE IF NOT EXISTS org_audit (
		id BIGSERIAL PRIMARY KEY,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		detail JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
}

func (s *Store) migrateOrg(ctx context.Context) error {
	for _, stmt := range orgMigrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("org migration failed: %w", err)
		}
	}
	return nil
}

// SlugNorm normalizes a free-form identifier to a lowercase, underscore-safe
// slug. Retained as a general utility.
func SlugNorm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
	for strings.Contains(s, "__") { // "José" folds to "jos__" — collapse runs
		s = strings.ReplaceAll(s, "__", "_")
	}
	return strings.Trim(s, "_")
}

// Audit records one actor's action. Every write path calls it with the REAL
// session identity (the handler resolves the impersonation header), so "who
// was really editing" survives admin view-as.
func (s *Store) Audit(ctx context.Context, actor, action, target string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		if enc, err := json.Marshal(detail); err == nil {
			b = enc
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_audit (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, string(b))
	return err
}

// auditTx is Audit inside the caller's transaction: a write and its audit row
// commit or roll back together.
func (s *Store) auditTx(ctx context.Context, ex execer, actor, action, target string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		if enc, err := json.Marshal(detail); err == nil {
			b = enc
		}
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO org_audit (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, string(b))
	return err
}
