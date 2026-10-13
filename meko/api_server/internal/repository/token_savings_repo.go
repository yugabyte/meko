package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// TokenSavingsRepo encapsulates read access to meko_system.token_savings_summary
// — running counters upserted at write time by meko-mcp-server's
// contextual_prominence_search MCP tool (see _record_token_savings). This is
// a lifetime counter, like meko_system.free_tier_usage: no per-user or
// time-range filtering. The detailed append-only log
// (meko_system.token_savings_log) is available for ad-hoc/audit queries but
// is never read here — YugabyteDB has no cheap index-only COUNT/SUM the way
// single-node Postgres does, so aggregating a growing log on every read
// doesn't scale the way a maintained counter does.
type TokenSavingsRepo struct {
	db *sql.DB
}

// NewTokenSavingsRepo creates a new TokenSavingsRepo.
func NewTokenSavingsRepo(db *sql.DB) *TokenSavingsRepo {
	return &TokenSavingsRepo{db: db}
}

// GetSummary reads meko_system.token_savings_summary for the given
// datapack_ids and aggregates totals, by-source, and by-datapack in Go from
// the (datapack_id, source) rows. The per-datapack route passes one id.
// The account view is GetAccountSummary, which does not call this. A plain
// indexed lookup, no SUM/COUNT pushdown.
func (r *TokenSavingsRepo) GetSummary(ctx context.Context, datapackIDs []string) (*models.TokenSavingsSummary, error) {
	summary := &models.TokenSavingsSummary{
		BySource:   map[string]int{},
		ByDatapack: []models.DatapackTokenSavings{},
	}
	if len(datapackIDs) == 0 {
		return summary, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT datapack_id, source, request_count, tokens_saved_total
		 FROM meko_system.token_savings_summary
		 WHERE datapack_id = ANY($1::uuid[])`,
		pq.Array(datapackIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to read token savings summary: %w", err)
	}
	defer rows.Close()

	// Pre-populate every requested datapack (even ones with zero rows in the
	// summary table) so the response always has one by_datapack entry per
	// requested id, and iterating datapackIDs at the end preserves the
	// caller's order rather than row-arrival order.
	byDatapack := make(map[string]*models.DatapackTokenSavings, len(datapackIDs))
	for _, id := range datapackIDs {
		byDatapack[id] = &models.DatapackTokenSavings{DatapackID: id}
	}

	for rows.Next() {
		var datapackID, source string
		var requestCount, tokensSaved int
		if err := rows.Scan(&datapackID, &source, &requestCount, &tokensSaved); err != nil {
			return nil, fmt.Errorf("failed to scan token savings summary row: %w", err)
		}

		summary.TotalRequests += requestCount
		summary.TotalTokensSaved += tokensSaved
		summary.BySource[source] += tokensSaved

		entry, ok := byDatapack[datapackID]
		if !ok {
			// Defensive: a row for an id outside the requested set shouldn't
			// happen given the WHERE clause, but don't drop it silently.
			entry = &models.DatapackTokenSavings{DatapackID: datapackID}
			byDatapack[datapackID] = entry
		}
		entry.TotalRequests += requestCount
		entry.TotalTokensSaved += tokensSaved
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate token savings summary: %w", err)
	}

	for _, id := range datapackIDs {
		summary.ByDatapack = append(summary.ByDatapack, *byDatapack[id])
	}

	return summary, nil
}

// GetAccountSummary is GetSummary over every datapack userID can see (owned,
// plus shared by the datapack list's definition), resolved in SQL rather than
// from an id list the caller would have to load first.
//
// ByDatapack lists only datapacks with savings, most tokens saved first, at
// most topN of them; ByDatapackTruncated says whether more exist. The totals
// cover every visible datapack.
func (r *TokenSavingsRepo) GetAccountSummary(
	ctx context.Context, userID string, topN int,
) (*models.TokenSavingsSummary, error) {
	rows, err := r.db.QueryContext(ctx,
		`WITH `+sharedInvitesCTE+`,
		 visible AS (
		     SELECT d.datapack_id FROM meko_system.datapacks d WHERE d.user_id = $1
		     UNION ALL
		     SELECT d.datapack_id FROM shared s
		     JOIN meko_system.datapacks d ON d.datapack_id = s.datapack_id
		     WHERE d.user_id <> $1
		 )
		 SELECT t.datapack_id, t.source, t.request_count, t.tokens_saved_total
		 FROM visible v
		 JOIN meko_system.token_savings_summary t ON t.datapack_id = v.datapack_id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to read account token savings: %w", err)
	}
	defer rows.Close()

	summary := &models.TokenSavingsSummary{
		BySource:   map[string]int{},
		ByDatapack: []models.DatapackTokenSavings{},
	}
	byDatapack := map[string]*models.DatapackTokenSavings{}
	for rows.Next() {
		var datapackID, source string
		var requestCount, tokensSaved int
		if err := rows.Scan(&datapackID, &source, &requestCount, &tokensSaved); err != nil {
			return nil, fmt.Errorf("failed to scan token savings summary row: %w", err)
		}
		summary.TotalRequests += requestCount
		summary.TotalTokensSaved += tokensSaved
		summary.BySource[source] += tokensSaved
		entry, found := byDatapack[datapackID]
		if !found {
			entry = &models.DatapackTokenSavings{DatapackID: datapackID}
			byDatapack[datapackID] = entry
		}
		entry.TotalRequests += requestCount
		entry.TotalTokensSaved += tokensSaved
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate token savings summary: %w", err)
	}

	for _, e := range byDatapack {
		summary.ByDatapack = append(summary.ByDatapack, *e)
	}
	sort.Slice(summary.ByDatapack, func(i, j int) bool {
		a, b := summary.ByDatapack[i], summary.ByDatapack[j]
		if a.TotalTokensSaved != b.TotalTokensSaved {
			return a.TotalTokensSaved > b.TotalTokensSaved
		}
		return a.DatapackID < b.DatapackID
	})
	if len(summary.ByDatapack) > topN {
		summary.ByDatapack = summary.ByDatapack[:topN]
		summary.ByDatapackTruncated = true
	}
	return summary, nil
}
