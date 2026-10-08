package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AdminFeedbackRow is the admin projection of one meko_system.feedback
// row. Mirrors the user-facing FeedbackListItem shape but stays
// package-local so the two dashboards can evolve independently — the
// admin console will want status transitions and internal notes
// eventually; the user-facing list will not.
type AdminFeedbackRow struct {
	ID               string
	CreatedAt        time.Time
	UpdatedAt        *time.Time
	UserID           *string
	Source           string
	SourceIdentifier *string
	DatapackID       *string
	DatapackName     *string
	ContextRoute     *string
	Category         string
	Body             string
	MekoVersion      *string
	Status           string
}

// AdminFeedbackPage bundles a slice of rows with the total-matching
// count so the caller can render "Showing 1-50 of N" without a
// separate query. Total comes from COUNT(*) OVER () in the same
// statement — one round trip per page.
type AdminFeedbackPage struct {
	Items []AdminFeedbackRow
	Total int64
}

// ListFeedbackByDateRange returns feedback rows created between
// [start, endExclusive) — inclusive lower bound, exclusive upper — with
// LIMIT/OFFSET pagination, newest first, and the total match count in
// the same round trip.
//
// Callers pass endExclusive already normalised to (to + 1 day) so this
// method stays a pure timestamp comparison (no date-vs-timestamp
// arithmetic buried in SQL). Bounds are optional: nil ⇒ unbounded on
// that side. limit is caller-clamped (handler does the 1..100 clamp
// and default-50); offset is a non-negative index.
//
// Rows and total come back in one query using COUNT(*) OVER (), so
// pagination display stays consistent even under concurrent inserts.
func (r *AdminRepo) ListFeedbackByDateRange(
	ctx context.Context,
	start, endExclusive *time.Time,
	limit, offset int,
) (AdminFeedbackPage, error) {
	pred, args := rangePredicate("created_at", start, endExclusive, 1)
	// $n placeholders for LIMIT / OFFSET follow the range predicate,
	// so their indices are len(args)+1 / len(args)+2 respectively.
	limitIdx := len(args) + 1
	offsetIdx := len(args) + 2

	q := `SELECT
			id,
			created_at,
			updated_at,
			user_id::text,
			source::text,
			source_identifier,
			datapack_id::text,
			datapack_name,
			context_route,
			category::text,
			body,
			meko_version,
			status::text,
			COUNT(*) OVER () AS total_count
		FROM meko_system.feedback
		WHERE TRUE` + pred + `
		ORDER BY created_at DESC
		LIMIT $` + strconv.Itoa(limitIdx) + ` OFFSET $` + strconv.Itoa(offsetIdx)

	args = append(args, limit, offset)

	rows, err := r.systemDB.QueryContext(ctx, q, args...)
	if err != nil {
		return AdminFeedbackPage{}, fmt.Errorf("list admin feedback: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := AdminFeedbackPage{Items: make([]AdminFeedbackRow, 0, limit)}
	for rows.Next() {
		var (
			row       AdminFeedbackRow
			updatedAt sql.NullTime
			userID    sql.NullString
			srcID     sql.NullString
			dpID      sql.NullString
			dpName    sql.NullString
			ctxRoute  sql.NullString
			mekoVer   sql.NullString
			total     int64
		)
		if scanErr := rows.Scan(
			&row.ID,
			&row.CreatedAt,
			&updatedAt,
			&userID,
			&row.Source,
			&srcID,
			&dpID,
			&dpName,
			&ctxRoute,
			&row.Category,
			&row.Body,
			&mekoVer,
			&row.Status,
			&total,
		); scanErr != nil {
			return AdminFeedbackPage{}, fmt.Errorf("scan admin feedback row: %w", scanErr)
		}
		if updatedAt.Valid {
			tm := updatedAt.Time
			row.UpdatedAt = &tm
		}
		if userID.Valid && strings.TrimSpace(userID.String) != "" {
			s := userID.String
			row.UserID = &s
		}
		if srcID.Valid && strings.TrimSpace(srcID.String) != "" {
			s := srcID.String
			row.SourceIdentifier = &s
		}
		if dpID.Valid && strings.TrimSpace(dpID.String) != "" {
			s := dpID.String
			row.DatapackID = &s
		}
		if dpName.Valid && strings.TrimSpace(dpName.String) != "" {
			s := dpName.String
			row.DatapackName = &s
		}
		if ctxRoute.Valid && strings.TrimSpace(ctxRoute.String) != "" {
			s := ctxRoute.String
			row.ContextRoute = &s
		}
		if mekoVer.Valid && strings.TrimSpace(mekoVer.String) != "" {
			s := mekoVer.String
			row.MekoVersion = &s
		}
		// total is identical on every row (window function); grabbing
		// the last-seen value is fine.
		page.Total = total
		page.Items = append(page.Items, row)
	}
	if err := rows.Err(); err != nil {
		return AdminFeedbackPage{}, fmt.Errorf("admin feedback rows: %w", err)
	}
	// COUNT(*) OVER () only surfaces on returned rows. When the window
	// is empty we still want a definitive total = 0 for pagination
	// display; the query above naturally returns nothing in that case
	// and page.Total stays zero, which matches the intent.
	return page, nil
}
