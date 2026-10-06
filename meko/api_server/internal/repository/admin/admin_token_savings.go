package admin

import (
	"context"
	"fmt"
	"time"
)

// TokenSavingsUserRow is one user's rolled-up token savings, attributed to the
// user who actually ran the searches (meko_system.token_savings_log.user_id),
// regardless of which datapack the savings occurred on. RequestCount is that
// user's number of contextual-search results; TokensSaved is their total.
type TokenSavingsUserRow struct {
	UserID       string
	Email        string
	FirstName    string
	LastName     string
	RequestCount int64
	TokensSaved  int64
}

// TotalTokenSavings sums the maintained token_savings_summary counters across
// every datapack in this deployment. token_savings_summary lives in the
// per-deployment meko_system schema, so a plain SUM carries no cross-deployment
// leak risk (unlike the shared Langfuse DB). Returns (0, 0) when no
// contextual-search requests have been recorded yet.
func (r *AdminRepo) TotalTokenSavings() (requests, tokensSaved int64, err error) {
	return r.TotalTokenSavingsContext(context.Background())
}

// TotalTokenSavingsContext is TotalTokenSavings under the caller's context, so a
// metrics scrape can bound the query with a deadline the pool has no
// statement_timeout to enforce on its own.
func (r *AdminRepo) TotalTokenSavingsContext(ctx context.Context) (requests, tokensSaved int64, err error) {
	if err = r.systemDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(request_count), 0), COALESCE(SUM(tokens_saved_total), 0)
		 FROM meko_system.token_savings_summary`,
	).Scan(&requests, &tokensSaved); err != nil {
		return 0, 0, fmt.Errorf("sum token savings: %w", err)
	}
	return requests, tokensSaved, nil
}

// TotalTokenSavingsInRange windows token savings by aggregating the append-only
// meko_system.token_savings_log on created_at (the summary counter carries no
// time dimension). Each log row is one contextual-search request, so COUNT(*)
// is the request count and SUM(tokens_saved) the tokens saved.
//
// This is a bounded distributed scan, NOT index-backed: token_savings_log is
// hash-distributed on id (000007), and a range index on the monotonic created_at
// would concentrate every insert on the tail tablet — the write skew 000007
// deliberately avoided — so we do not add one. Acceptable because the dashboard
// is an admin-only read (client-cached 60s). nil bounds mean all-time, but
// prefer TotalTokenSavings there (it reads the cheaper maintained counters).
func (r *AdminRepo) TotalTokenSavingsInRange(start, endExclusive *time.Time) (requests, tokensSaved int64, err error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	if err = r.systemDB.QueryRow(
		`SELECT COUNT(*)::bigint, COALESCE(SUM(tokens_saved), 0)::bigint
		 FROM meko_system.token_savings_log
		 WHERE TRUE`+pred,
		args...,
	).Scan(&requests, &tokensSaved); err != nil {
		return 0, 0, fmt.Errorf("sum token savings (range): %w", err)
	}
	return requests, tokensSaved, nil
}

// TopUserTokenSavings returns the top `limit` users by tokens saved in the
// optional [start, endExclusive) window, attributed to the user who ran each
// search (token_savings_log.user_id) — not the datapack owner — so a user is
// credited wherever they saved, regardless of datapack. No minimum-request
// threshold: any user who saved tokens is eligible, ordered by tokens saved.
//
// This aggregates the append-only token_savings_log (the only place the actual
// searcher is recorded; token_savings_summary carries no user_id). Aggregating
// the growing log is deliberately avoided on the hot per-request read path.
//
// Performance: this is a distributed aggregate over the log — the created_at
// range narrows it, but the JOIN to users + GROUP BY on four of its columns
// could never be index-only anyway, and we deliberately do NOT index the
// monotonic created_at (a range index there would hot-spot the write tail,
// the skew 000007 avoids). Acceptable because it is an admin-only read
// (client-cached 60s; no server-side cache). The UNBOUNDED call (meko_ui's
// legacy /admin/token-savings, nil bounds) scans the whole log and goes away
// when meko_ui moves to the ranged panels.
func (r *AdminRepo) TopUserTokenSavings(start, endExclusive *time.Time, limit int) ([]TokenSavingsUserRow, error) {
	// Range params (if any) take $1..; limit follows them.
	pred, rangeArgs := rangePredicate("l.created_at", start, endExclusive, 1)
	args := append(rangeArgs, limit)
	rows, err := r.systemDB.Query(
		fmt.Sprintf(`SELECT l.user_id::text,
		        COALESCE(u.user_email, ''),
		        COALESCE(u.user_first_name, ''),
		        COALESCE(u.user_last_name, ''),
		        COUNT(*)            AS request_count,
		        SUM(l.tokens_saved) AS tokens_saved
		 FROM meko_system.token_savings_log l
		 JOIN meko_system.users u ON u.user_id = l.user_id
		 WHERE l.user_id IS NOT NULL%s
		 GROUP BY l.user_id, u.user_email, u.user_first_name, u.user_last_name
		 ORDER BY tokens_saved DESC
		 LIMIT $%d`, pred, len(rangeArgs)+1),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("top user token savings: %w", err)
	}
	defer rows.Close()

	out := []TokenSavingsUserRow{}
	for rows.Next() {
		var row TokenSavingsUserRow
		if err := rows.Scan(
			&row.UserID, &row.Email, &row.FirstName, &row.LastName,
			&row.RequestCount, &row.TokensSaved,
		); err != nil {
			return nil, fmt.Errorf("scan top user token savings: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top user token savings: %w", err)
	}
	return out, nil
}
