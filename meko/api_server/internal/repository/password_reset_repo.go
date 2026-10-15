package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"meko/api_server/internal/models"
)

// passwordResetRetention is how long request rows are kept. The service's
// limit window and code lifetime are both shorter.
const passwordResetRetention = 24 * time.Hour

// passwordResetPruneInterval is how often each api_server pod deletes rows
// older than passwordResetRetention for all emails. RecordResetRequest also
// deletes the requesting email's old rows every time.
const passwordResetPruneInterval = time.Hour

// PasswordResetRepo wraps meko_system.password_reset_requests: the per-email
// forgot-password limit and the reset codes Meko issues itself (MEKO-887).
// Callers pass the email already lowercased.
type PasswordResetRepo struct {
	lastPrune time.Time
	db        *sql.DB
	pruneMu   sync.Mutex
}

// NewPasswordResetRepo creates a PasswordResetRepo on the system database.
func NewPasswordResetRepo(db *sql.DB) *PasswordResetRepo {
	return &PasswordResetRepo{db: db}
}

// CountResetRequestsSince returns how many requests email made at or after
// since.
func (r *PasswordResetRepo) CountResetRequestsSince(ctx context.Context, email string, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM meko_system.password_reset_requests
		 WHERE email = $1 AND requested_at >= $2`,
		email, since,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("password_reset_requests: count for email: %w", err)
	}
	return n, nil
}

// RecordResetRequest stores a new request for email and returns its id. It
// first deletes the email's rows older than the retention period, and at
// most once per passwordResetPruneInterval per pod, every email's.
func (r *PasswordResetRepo) RecordResetRequest(ctx context.Context, email string) (string, error) {
	cutoff := time.Now().Add(-passwordResetRetention)
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.password_reset_requests
		 WHERE email = $1 AND requested_at < $2`,
		email, cutoff,
	); err != nil {
		return "", fmt.Errorf("password_reset_requests: prune for email: %w", err)
	}
	r.pruneAllIfDue(ctx, cutoff)
	var id string
	if err := r.db.QueryRowContext(ctx,
		`INSERT INTO meko_system.password_reset_requests (email) VALUES ($1) RETURNING id`,
		email,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("password_reset_requests: insert: %w", err)
	}
	return id, nil
}

// AttachResetCode stores a Meko-issued code on request id. It runs after
// RecordResetRequest, so the email's earlier codes are already superseded:
// LatestResetRequest only ever returns the newest row.
func (r *PasswordResetRepo) AttachResetCode(
	ctx context.Context, email, id, codeHash string, expiresAt time.Time,
) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.password_reset_requests
		 SET code_hash = $3, expires_at = $4
		 WHERE email = $1 AND id = $2`,
		email, id, codeHash, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("password_reset_requests: attach code: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("password_reset_requests: attach code: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("password_reset_requests: attach code: %d rows updated, want 1", n)
	}
	return nil
}

// LatestResetRequest returns the email's newest request, or nil when it has
// none. Rows older than passwordResetRetention are usually pruned already.
func (r *PasswordResetRepo) LatestResetRequest(
	ctx context.Context, email string,
) (*models.PasswordResetRequest, error) {
	var (
		req      models.PasswordResetRequest
		codeHash sql.NullString
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT id, email, requested_at, code_hash, expires_at, used_at, attempts
		 FROM meko_system.password_reset_requests
		 WHERE email = $1
		 ORDER BY requested_at DESC
		 LIMIT 1`,
		email,
	).Scan(&req.ID, &req.Email, &req.RequestedAt, &codeHash, &req.ExpiresAt, &req.UsedAt, &req.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("password_reset_requests: latest for email: %w", err)
	}
	req.CodeHash = codeHash.String
	return &req, nil
}

// ReserveResetAttempt uses one of request id's maxAttempts guesses and reports
// whether it got one. It refuses a code that is used, expired, or out of
// guesses. The check and the increment are one UPDATE, which locks the row,
// so concurrent guesses can't exceed maxAttempts between them.
func (r *PasswordResetRepo) ReserveResetAttempt(ctx context.Context, email, id string, maxAttempts int) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.password_reset_requests
		 SET attempts = attempts + 1
		 WHERE email = $1 AND id = $2 AND code_hash IS NOT NULL AND used_at IS NULL
		   AND expires_at > NOW() AND attempts < $3`,
		email, id, maxAttempts,
	)
	if err != nil {
		return false, fmt.Errorf("password_reset_requests: reserve attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("password_reset_requests: reserve attempt: %w", err)
	}
	return n == 1, nil
}

// ClaimResetCode marks request id used and reports whether this call claimed
// it. Only one caller can claim a code, which makes it single-use. Callers that
// claim before Cognito accepts a new password must UnclaimResetCode on failure.
func (r *PasswordResetRepo) ClaimResetCode(ctx context.Context, email, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.password_reset_requests
		 SET used_at = NOW()
		 WHERE email = $1 AND id = $2 AND used_at IS NULL`,
		email, id,
	)
	if err != nil {
		return false, fmt.Errorf("password_reset_requests: claim code: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("password_reset_requests: claim code: %w", err)
	}
	return n == 1, nil
}

// UnclaimResetCode clears used_at on request id so a failed Cognito set can be
// retried with the same code. The reserved attempt still counts.
func (r *PasswordResetRepo) UnclaimResetCode(ctx context.Context, email, id string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.password_reset_requests
		 SET used_at = NULL
		 WHERE email = $1 AND id = $2`,
		email, id,
	); err != nil {
		return fmt.Errorf("password_reset_requests: unclaim code: %w", err)
	}
	return nil
}

// DeleteResetRequest removes request id. Used when a recorded request is
// rejected or abandoned so it neither counts toward the per-email limit nor
// hides an earlier live Meko code from confirm.
func (r *PasswordResetRepo) DeleteResetRequest(ctx context.Context, email, id string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.password_reset_requests WHERE email = $1 AND id = $2`,
		email, id,
	); err != nil {
		return fmt.Errorf("password_reset_requests: delete request: %w", err)
	}
	return nil
}

// pruneAllIfDue deletes every email's rows older than cutoff, at most once per
// passwordResetPruneInterval per pod. The requested_at index keeps it a range
// scan. A failure is logged and retried at the next interval.
func (r *PasswordResetRepo) pruneAllIfDue(ctx context.Context, cutoff time.Time) {
	r.pruneMu.Lock()
	if time.Since(r.lastPrune) < passwordResetPruneInterval {
		r.pruneMu.Unlock()
		return
	}
	r.lastPrune = time.Now()
	r.pruneMu.Unlock()

	res, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.password_reset_requests WHERE requested_at < $1`,
		cutoff,
	)
	if err != nil {
		slog.Warn("password_reset_requests: global prune failed", "error", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		slog.Info("password_reset_requests: pruned old rows", "rows", n)
	}
}
