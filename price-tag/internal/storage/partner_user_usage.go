package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"
)

// PartnerUserUsage is one user's report with the directory tags supplied by
// the partner-user registry. UserID is stable; usage rows are joined through
// the user's current and historical MaaS login names.
type PartnerUserUsage struct {
	UserID string             `json:"user_id"`
	Tags   map[string]any     `json:"tags"`
	Active bool               `json:"active"`
	Totals UsageTotals        `json:"totals"`
	Models []ModelUsageReport `json:"models"`
}

type PartnerUsersUsageReport struct {
	From           time.Time          `json:"from"`
	To             time.Time          `json:"to"`
	AsOf           time.Time          `json:"as_of"`
	Users          []PartnerUserUsage `json:"users"`
	MissingUserIDs []string           `json:"missing_user_ids"`
}

// GetPartnerUsersUsageReport returns reports for requested SSO IDs over a
// half-open interval [from,to). It uses every MaaS username historically
// mapped to each UUID so an email/login change does not lose prior usage.
// Unknown IDs are returned separately; users without events have zero totals.
func (s *Store) GetPartnerUsersUsageReport(ctx context.Context, userIDs []string, from, to time.Time) (PartnerUsersUsageReport, error) {
	if !from.Before(to) {
		return PartnerUsersUsageReport{}, fmt.Errorf("from must be earlier than to")
	}
	ids := make([]string, 0, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))
	for _, raw := range userIDs {
		id, err := normalizePartnerUserID(raw)
		if err != nil {
			return PartnerUsersUsageReport{}, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return PartnerUsersUsageReport{}, fmt.Errorf("user_ids must contain at least one UUID")
	}

	report := PartnerUsersUsageReport{
		From: from.UTC(), To: to.UTC(), AsOf: time.Now().UTC(),
		Users: []PartnerUserUsage{}, MissingUserIDs: []string{},
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id::text,tags,role,manager_user_id::text,active,key_revocation_pending,created_at,updated_at
		FROM partner_users WHERE user_id = ANY($1::uuid[]) ORDER BY user_id`, pq.Array(ids))
	if err != nil {
		return PartnerUsersUsageReport{}, fmt.Errorf("load partner users: %w", err)
	}
	userIndex := make(map[string]int, len(ids))
	for rows.Next() {
		user, err := scanPartnerUser(rows)
		if err != nil {
			rows.Close()
			return PartnerUsersUsageReport{}, err
		}
		userIndex[user.UserID] = len(report.Users)
		report.Users = append(report.Users, PartnerUserUsage{
			UserID: user.UserID,
			Tags:   user.Tags,
			Active: user.Active,
			Models: []ModelUsageReport{},
		})
	}
	if err := rows.Close(); err != nil {
		return PartnerUsersUsageReport{}, err
	}
	for _, id := range ids {
		if _, ok := userIndex[id]; !ok {
			report.MissingUserIDs = append(report.MissingUserIDs, id)
		}
	}
	if len(report.Users) == 0 {
		return report, nil
	}

	// This report predates usage_hourly and exposes reasoning_tokens, which the
	// rollup does not carry. Keep the raw aggregate (one grouped scan) but run
	// it on the read replica so partner reporting cannot load the primary.
	usageRows, err := s.reader().QueryContext(ctx, fmt.Sprintf(`
		SELECT l.user_id::text, COALESCE(e.provider,''), COALESCE(e.model,''),
			COUNT(e.id)::bigint,
			COALESCE(SUM(e.prompt_tokens),0)::bigint,
			COALESCE(SUM(e.completion_tokens),0)::bigint,
			COALESCE(SUM(e.total_tokens),0)::bigint,
			COALESCE(SUM(e.cached_input_tokens),0)::bigint,
			COALESCE(SUM(e.cache_creation_tokens),0)::bigint,
			COALESCE(SUM(e.reasoning_tokens),0)::bigint,
			COALESCE(SUM(%s),0)
		FROM partner_user_logins l
		LEFT JOIN usage_events e ON e.username=l.username AND e.timestamp >= $2 AND e.timestamp < $3
		LEFT JOIN model_pricing p ON p.model=e.model
		WHERE l.user_id = ANY($1::uuid[])
		GROUP BY l.user_id, COALESCE(e.provider,''), COALESCE(e.model,'')
		ORDER BY l.user_id, COALESCE(e.provider,''), COALESCE(e.model,'')`, costUSDExpr), pq.Array(ids), from.UTC(), to.UTC())
	if err != nil {
		return PartnerUsersUsageReport{}, fmt.Errorf("query partner user usage: %w", err)
	}
	defer usageRows.Close()
	for usageRows.Next() {
		var userID string
		var model ModelUsageReport
		if err := usageRows.Scan(
			&userID, &model.Provider, &model.Model,
			&model.Requests, &model.PromptTokens, &model.CompletionTokens,
			&model.TotalTokens, &model.CachedInputTokens, &model.CacheCreationTokens,
			&model.ReasoningTokens, &model.EstimatedCostUSD,
		); err != nil {
			return PartnerUsersUsageReport{}, err
		}
		idx, ok := userIndex[userID]
		if !ok {
			continue
		}
		if model.Requests == 0 && model.Model == "" && model.Provider == "" {
			continue
		}
		user := &report.Users[idx]
		user.Models = append(user.Models, model)
		user.Totals.Requests += model.Requests
		user.Totals.PromptTokens += model.PromptTokens
		user.Totals.CompletionTokens += model.CompletionTokens
		user.Totals.TotalTokens += model.TotalTokens
		user.Totals.CachedInputTokens += model.CachedInputTokens
		user.Totals.CacheCreationTokens += model.CacheCreationTokens
		user.Totals.ReasoningTokens += model.ReasoningTokens
		user.Totals.EstimatedCostUSD += model.EstimatedCostUSD
	}
	if err := usageRows.Err(); err != nil {
		return PartnerUsersUsageReport{}, err
	}
	sort.Strings(report.MissingUserIDs)
	return report, nil
}
