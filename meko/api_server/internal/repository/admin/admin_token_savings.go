package admin

import "fmt"

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
	if err = r.systemDB.QueryRow(
		`SELECT COALESCE(SUM(request_count), 0), COALESCE(SUM(tokens_saved_total), 0)
		 FROM meko_system.token_savings_summary`,
	).Scan(&requests, &tokensSaved); err != nil {
		return 0, 0, fmt.Errorf("sum token savings: %w", err)
	}
	return requests, tokensSaved, nil
}

// TopUserTokenSavings returns the top `limit` users by lifetime tokens saved,
// attributed to the user who ran each search (token_savings_log.user_id) — not
// the datapack owner — so a user is credited wherever they saved, regardless of
// datapack. Restricted to users with at least `minRequests` searches (sustained
// adopters, not one-off spikes) and ordered by tokens saved, descending.
//
// This aggregates the append-only token_savings_log (the only place the actual
// searcher is recorded; token_savings_summary carries no user_id). Aggregating
// the growing log is deliberately avoided on the hot per-request read path, but
// is fine here: the admin executive dashboard is infrequent and cached.
func (r *AdminRepo) TopUserTokenSavings(minRequests, limit int) ([]TokenSavingsUserRow, error) {
	rows, err := r.systemDB.Query(
		`SELECT l.user_id::text,
		        COALESCE(u.user_email, ''),
		        COALESCE(u.user_first_name, ''),
		        COALESCE(u.user_last_name, ''),
		        COUNT(*)            AS request_count,
		        SUM(l.tokens_saved) AS tokens_saved
		 FROM meko_system.token_savings_log l
		 JOIN meko_system.users u ON u.user_id = l.user_id
		 WHERE l.user_id IS NOT NULL
		 GROUP BY l.user_id, u.user_email, u.user_first_name, u.user_last_name
		 HAVING COUNT(*) >= $1
		 ORDER BY tokens_saved DESC
		 LIMIT $2`,
		minRequests, limit,
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
