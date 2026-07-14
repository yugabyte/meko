package admin

import (
	"context"
	"fmt"
	"time"
)

// AdminUserRow is a minimal user projection for the admin console:
// identity plus the raw user_type (mapped to a tier in the service).
type AdminUserRow struct {
	UserID    string
	Email     string
	FirstName string
	LastName  string
	UserType  string
	CreatedAt time.Time
}

// ListUsers returns all users ordered by created_at DESC for the admin
// console. Reads meko_system.users only.
func (r *AdminRepo) ListUsers(ctx context.Context) ([]AdminUserRow, error) {
	rows, err := r.systemDB.QueryContext(ctx,
		`SELECT user_id, user_email, user_first_name, user_last_name, user_type, created_at
		 FROM meko_system.users
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []AdminUserRow
	for rows.Next() {
		var u AdminUserRow
		if err := rows.Scan(
			&u.UserID, &u.Email, &u.FirstName, &u.LastName, &u.UserType, &u.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user row: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsersByTier returns (total, free, pro) user counts where "free"
// is user_type='free' and "pro" is everything else (pro/enterprise).
// When start/endExclusive are set, counts users created_at in the window.
func (r *AdminRepo) CountUsersByTier(
	ctx context.Context, start, endExclusive *time.Time,
) (total, free, pro int64, err error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	where := ""
	if pred != "" {
		where = " WHERE TRUE" + pred
	}
	q := `SELECT
		   COUNT(*),
		   COUNT(*) FILTER (WHERE user_type = 'free'),
		   COUNT(*) FILTER (WHERE user_type <> 'free')
		 FROM meko_system.users` + where
	if err = r.systemDB.QueryRowContext(ctx, q, args...).Scan(&total, &free, &pro); err != nil {
		return 0, 0, 0, fmt.Errorf("count users by tier: %w", err)
	}
	return total, free, pro, nil
}

// ListUserIDs returns every user_id in meko_system.users. Used to scope
// overall Langfuse aggregates to this deployment's users only.
//
// TODO(scaling): materializes all ids and passes them as an ANY($1) array to
// the Langfuse queries. Fine for current deployment sizes; at tens of
// thousands of users, switch to a server-side join or cache the overall
// aggregates instead of enumerating ids.
func (r *AdminRepo) ListUserIDs(ctx context.Context) ([]string, error) {
	rows, err := r.systemDB.QueryContext(ctx,
		`SELECT user_id::text FROM meko_system.users`,
	)
	if err != nil {
		return nil, fmt.Errorf("list user ids: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpdateUserTier sets a user's user_type. Returns (true, nil) on a hit,
// (false, nil) when no user matched the id (caller maps to not-found).
func (r *AdminRepo) UpdateUserTier(ctx context.Context, userID, tier string) (bool, error) {
	res, err := r.systemDB.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET user_type = $1::meko_system.account_type, updated_at = NOW()
		 WHERE user_id = $2`,
		tier, userID,
	)
	if err != nil {
		return false, fmt.Errorf("update user tier: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update user tier rows: %w", err)
	}
	return n > 0, nil
}

// GetUserType returns the raw user_type for a single user, or "" and
// sql.ErrNoRows when the user does not exist.
func (r *AdminRepo) GetUserType(ctx context.Context, userID string) (string, error) {
	var t string
	if err := r.systemDB.QueryRowContext(ctx,
		`SELECT user_type FROM meko_system.users WHERE user_id = $1`,
		userID,
	).Scan(&t); err != nil {
		return "", err
	}
	return t, nil
}
