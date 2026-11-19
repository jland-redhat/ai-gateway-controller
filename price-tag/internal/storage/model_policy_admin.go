package storage

import (
	"context"
	"time"

	"github.com/lib/pq"
)

// PartnerUserModelPolicyRow is the operator-facing view of a model restriction.
// A row exists only when the external model-policy API has installed an
// allowlist; no row means the user follows the normal gateway catalog.
type PartnerUserModelPolicyRow struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	Models    []string  `json:"models"`
	DenyAll   bool      `json:"deny_all"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListPartnerUserModelPolicies returns users with an explicit external
// allowlist. It intentionally does not include users without a policy row.
func (s *Store) ListPartnerUserModelPolicies(ctx context.Context, limit, offset int) ([]PartnerUserModelPolicyRow, int, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM partner_user_model_allowlists`).Scan(&total); err != nil {
		return nil, 0, false, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT a.user_id::text,
		       COALESCE(p.tags->>'email', ''),
		       TRIM(CONCAT_WS(' ', p.tags->>'first_name', p.tags->>'last_name')),
		       p.active, a.models, a.updated_by, a.updated_at
		FROM partner_user_model_allowlists a
		JOIN partner_users p ON p.user_id = a.user_id
		ORDER BY a.updated_at DESC, a.user_id
		LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()

	result := make([]PartnerUserModelPolicyRow, 0, limit+1)
	for rows.Next() {
		var row PartnerUserModelPolicyRow
		if err := rows.Scan(&row.UserID, &row.Email, &row.Name, &row.Active, pq.Array(&row.Models), &row.UpdatedBy, &row.UpdatedAt); err != nil {
			return nil, 0, false, err
		}
		row.DenyAll = len(row.Models) == 0
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, total, hasMore, nil
}
