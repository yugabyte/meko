package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AuditActor identifies the operator behind an admin action. Email is the
// human-readable identity; UUID is the operator's stable id (Cognito sub) and
// may be empty for automated sources.
type AuditActor struct {
	Email string
	UUID  string
}

// ErrNoActor is returned when an audited mutation is attempted without an
// operator identity — we refuse to record an un-attributable admin action.
var ErrNoActor = errors.New("audit: actor email is required")

// TierUpdate is the outcome of an audited tier override.
type TierUpdate struct {
	Found   bool   // did a user match the id?
	Changed bool   // was the tier actually different (and thus updated + audited)?
	OldTier string // the tier before the change (empty when !Found)
}

// TierAuditRow is one tier-change event projected from admin_audit_log.
type TierAuditRow struct {
	ActorEmail string
	ActorUUID  string
	Source     string
	OldTier    string
	NewTier    string
	CreatedAt  time.Time
}

// insertAuditLogTx appends one row to admin_audit_log within an open
// transaction, so the log and the mutation it records commit together.
func insertAuditLogTx(
	ctx context.Context, tx *sql.Tx, actor AuditActor,
	source, action, targetType, targetID string, details map[string]any,
) error {
	payload, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal audit details: %w", err)
	}
	var uuid any
	if actor.UUID != "" {
		uuid = actor.UUID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meko_system.admin_audit_log
		   (actor_email, actor_uuid, source, action, target_type, target_id, details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		actor.Email, uuid, source, action, targetType, targetID, string(payload),
	); err != nil {
		return fmt.Errorf("insert admin_audit_log: %w", err)
	}
	return nil
}

// UpdateUserTierAudited overrides a user's tier and records the change in
// admin_audit_log atomically (same transaction), so the two can never diverge.
// Returns Found=false when no user matches the id, and Changed=false when the
// tier already equals newTier (a no-op — nothing is written, nothing audited).
func (r *AdminRepo) UpdateUserTierAudited(
	ctx context.Context, userID, newTier string, actor AuditActor,
) (TierUpdate, error) {
	if actor.Email == "" {
		return TierUpdate{}, ErrNoActor
	}
	tx, err := r.systemDB.BeginTx(ctx, nil)
	if err != nil {
		return TierUpdate{}, fmt.Errorf("begin tier audit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldTier string
	err = tx.QueryRowContext(ctx,
		`SELECT user_type FROM meko_system.users WHERE user_id = $1 FOR UPDATE`,
		userID,
	).Scan(&oldTier)
	if errors.Is(err, sql.ErrNoRows) {
		return TierUpdate{Found: false}, nil
	}
	if err != nil {
		return TierUpdate{}, fmt.Errorf("read current tier: %w", err)
	}
	if oldTier == newTier {
		// No change — commit the (empty) tx and skip the audit row.
		if err := tx.Commit(); err != nil {
			return TierUpdate{}, fmt.Errorf("commit tier audit tx: %w", err)
		}
		return TierUpdate{Found: true, Changed: false, OldTier: oldTier}, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET user_type = $1::meko_system.account_type, updated_at = NOW()
		 WHERE user_id = $2`,
		newTier, userID,
	); err != nil {
		return TierUpdate{}, fmt.Errorf("update user tier: %w", err)
	}

	if err := insertAuditLogTx(ctx, tx, actor,
		"admin_console", "user.tier.update", "user", userID,
		map[string]any{"old_tier": oldTier, "new_tier": newTier},
	); err != nil {
		return TierUpdate{}, err
	}

	if err := tx.Commit(); err != nil {
		return TierUpdate{}, fmt.Errorf("commit tier audit tx: %w", err)
	}
	return TierUpdate{Found: true, Changed: true, OldTier: oldTier}, nil
}

// ListUserTierAudit returns a user's tier-change history (newest first) from
// admin_audit_log. Capped so a long-lived account can't return an unbounded page.
func (r *AdminRepo) ListUserTierAudit(ctx context.Context, userID string) ([]TierAuditRow, error) {
	rows, err := r.systemDB.QueryContext(ctx,
		`SELECT actor_email, COALESCE(actor_uuid, ''), source,
		        COALESCE(details->>'old_tier', ''), COALESCE(details->>'new_tier', ''),
		        created_at
		 FROM   meko_system.admin_audit_log
		 WHERE  action = 'user.tier.update' AND target_type = 'user' AND target_id = $1
		 ORDER  BY created_at DESC
		 LIMIT  200`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tier audit: %w", err)
	}
	defer rows.Close()

	var out []TierAuditRow
	for rows.Next() {
		var e TierAuditRow
		if err := rows.Scan(&e.ActorEmail, &e.ActorUUID, &e.Source, &e.OldTier, &e.NewTier, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tier audit row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
