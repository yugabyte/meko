package admin

import (
	"context"
	"fmt"
	"time"
)

// Data-plane table names for the memory counters. These mirror the
// constants used by the shared repositories; duplicated here so the admin
// package stays self-contained.
const (
	memoriesTableSQL        = `datapacks.meko_mem0_collection`
	promotedCollectionTable = `datapacks.promoted_collection`
)

// CountPrivateMemoriesAll returns the global number of private-memory rows
// across all datapacks (overall admin metric). start/endExclusive optionally
// window by meko_created_at.
func (r *AdminRepo) CountPrivateMemoriesAll(ctx context.Context, start, endExclusive *time.Time) (int64, error) {
	if r.dataDB == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	pred, args := rangePredicate("meko_created_at", start, endExclusive, 1)
	where := ""
	if pred != "" {
		where = " WHERE TRUE" + pred
	}
	var n int64
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, memoriesTableSQL) + where
	if err := r.dataDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("memories count all: %w", err)
	}
	return n, nil
}

// CountPrivateMemoriesByUser returns the number of private-memory rows owned
// by a user (per-user admin metric), scoped by meko_user_id. start/endExclusive
// optionally window by meko_created_at.
func (r *AdminRepo) CountPrivateMemoriesByUser(ctx context.Context, userID string, start, endExclusive *time.Time) (int64, error) {
	if r.dataDB == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	pred, rangeArgs := rangePredicate("meko_created_at", start, endExclusive, 2)
	args := append([]any{userID}, rangeArgs...)
	var n int64
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE meko_user_id = $1::uuid`, memoriesTableSQL) + pred
	if err := r.dataDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("memories count by user: %w", err)
	}
	return n, nil
}

// CountAllPromoted returns the global number of collective (promoted)
// memory rows across all datapacks (overall admin metric). start/
// endExclusive optionally window by meko_promoted_to_kb_at.
func (r *AdminRepo) CountAllPromoted(ctx context.Context, start, endExclusive *time.Time) (int64, error) {
	if r.dataDB == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	pred, args := rangePredicate("meko_promoted_to_kb_at", start, endExclusive, 1)
	where := ""
	if pred != "" {
		where = " WHERE TRUE" + pred
	}
	var n int64
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, promotedCollectionTable) + where
	if err := r.dataDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("promoted count all: %w", err)
	}
	return n, nil
}

// CountPromotedByUser returns the number of collective (promoted)
// memory rows owned by a user (per-user admin metric). start/endExclusive
// optionally window by meko_promoted_to_kb_at.
func (r *AdminRepo) CountPromotedByUser(ctx context.Context, userID string, start, endExclusive *time.Time) (int64, error) {
	if r.dataDB == nil {
		return 0, fmt.Errorf("database connection is required")
	}
	pred, rangeArgs := rangePredicate("meko_promoted_to_kb_at", start, endExclusive, 2)
	args := append([]any{userID}, rangeArgs...)
	var n int64
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE meko_user_id = $1::uuid`, promotedCollectionTable) + pred
	if err := r.dataDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("promoted count by user: %w", err)
	}
	return n, nil
}
