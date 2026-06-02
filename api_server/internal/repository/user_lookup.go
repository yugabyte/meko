// Shared systemDB user-display lookups. Used by repos whose primary
// tables live on dataDB (memories, dist_rag documents, activity log,
// …) but whose list responses include the owner's name / email — i.e.
// every place that used to do `LEFT JOIN meko_system.users` inline
// before the systemDB / dataDB split.
//
// Free function (not a method) on purpose: the only state it needs is
// a `*sql.DB` for systemDB, and several repos hold that handle. Putting
// it on any one repo would force the others to call across repo
// boundaries, which is messier than passing the handle in.

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// UserDisplay holds the human-readable name + email for a user, used
// to enrich list responses. Both fields are empty strings when the
// underlying meko_system.users row has them as NULL / blank — callers
// typically attach with `omitempty` semantics.
type UserDisplay struct {
	Name  string
	Email string
}

// LookupUserDisplays batch-resolves user_id → {name, email} against
// meko_system.users. Empty / whitespace ids are dropped; the input is
// de-duplicated before the query (callers usually pass a list parallel
// to their result rows with repeats). Missing users are silently
// absent from the returned map — matches the previous LEFT JOIN
// semantics: no row → no enrichment → caller leaves the fields empty.
func LookupUserDisplays(
	ctx context.Context,
	systemDB *sql.DB,
	userIDs []string,
) (map[string]UserDisplay, error) {
	if len(userIDs) == 0 || systemDB == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(userIDs))
	unique := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := systemDB.QueryContext(
		queryCtx,
		`SELECT user_id::text,
		        COALESCE(NULLIF(TRIM(COALESCE(user_first_name, '') || ' ' || COALESCE(user_last_name, '')), ''), '') AS user_name,
		        COALESCE(user_email, '') AS user_email
		 FROM   meko_system.users
		 WHERE  user_id = ANY($1::uuid[])`,
		pq.Array(unique),
	)
	if err != nil {
		return nil, fmt.Errorf("lookup user displays: %w", err)
	}
	defer rows.Close()
	out := make(map[string]UserDisplay, len(unique))
	for rows.Next() {
		var (
			uid   string
			name  sql.NullString
			email sql.NullString
		)
		if err := rows.Scan(&uid, &name, &email); err != nil {
			return nil, fmt.Errorf("scan user display: %w", err)
		}
		out[uid] = UserDisplay{
			Name:  strings.TrimSpace(name.String),
			Email: strings.TrimSpace(email.String),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user display rows: %w", err)
	}
	return out, nil
}
