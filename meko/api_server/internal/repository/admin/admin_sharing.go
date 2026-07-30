package admin

import (
	"context"
	"fmt"
	"time"
)

// CountSharingOwners returns the total number of shares (accepted invites)
// across all users — the overall "shares" admin metric. start/endExclusive
// optionally window by the invite's created_at.
func (r *AdminRepo) CountSharingOwners(ctx context.Context, start, endExclusive *time.Time) (int64, error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	var n int64
	if err := r.systemDB.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM meko_system.datapack_invites
		 WHERE status = 'accepted'`+pred,
		args...,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count shares: %w", err)
	}
	return n, nil
}

// CountAcceptedSharesByOwner returns the number of shares (accepted invites)
// made by `userID` — the per-user "shares" admin metric. start/endExclusive
// optionally window by the invite's created_at.
func (r *AdminRepo) CountAcceptedSharesByOwner(ctx context.Context, userID string, start, endExclusive *time.Time) (int64, error) {
	pred, rangeArgs := rangePredicate("created_at", start, endExclusive, 2)
	args := append([]any{userID}, rangeArgs...)
	var n int64
	if err := r.systemDB.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM meko_system.datapack_invites
		 WHERE owner_user_id = $1 AND status = 'accepted'`+pred,
		args...,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count shares for user %s: %w", userID, err)
	}
	return n, nil
}
