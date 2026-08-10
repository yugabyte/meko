// Package admin: admin_user_service handles admin-initiated user
// mutations (currently tier overrides) for the meko_admin console.
package admin

import (
	"context"
	"errors"
	"time"

	adminrepo "meko/api_server/internal/repository/admin"
)

// ErrInvalidTier is returned when a tier update names a value outside the
// meko_system.account_type enum; the handler maps it to 400.
var ErrInvalidTier = errors.New("invalid tier")

// validTiers are the tiers the admin console may set: free / standard / pro.
// Enterprise is sales-led / off-platform and is intentionally NOT settable here.
var validTiers = map[string]bool{"free": true, "standard": true, "pro": true}

// UpdateUserTier overrides a user's user_type for admin/comp grants and records
// the change in the admin audit log (who, old→new, when) in one transaction.
// actorEmail/actorUUID identify the operator (the handler guarantees a non-empty
// actorEmail). Returns ErrInvalidTier for a tier outside {free, standard, pro}
// and ErrUserNotFound when the user does not exist.
func (s *AdminMetricsService) UpdateUserTier(ctx context.Context, userID, tier, actorEmail, actorUUID string) error {
	if !validTiers[tier] {
		return ErrInvalidTier
	}
	res, err := s.repo.UpdateUserTierAudited(ctx, userID, tier,
		adminrepo.AuditActor{Email: actorEmail, UUID: actorUUID})
	if err != nil {
		return err
	}
	if !res.Found {
		return ErrUserNotFound
	}
	return nil
}

// TierAuditEntry is one tier-change event for the admin console's history view.
type TierAuditEntry struct {
	ActorEmail string    `json:"actor_email"`
	ActorUUID  string    `json:"actor_uuid,omitempty"`
	Source     string    `json:"source"`
	OldTier    string    `json:"old_tier"`
	NewTier    string    `json:"new_tier"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListUserTierAudit returns a user's tier-change history, newest first.
func (s *AdminMetricsService) ListUserTierAudit(ctx context.Context, userID string) ([]TierAuditEntry, error) {
	rows, err := s.repo.ListUserTierAudit(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]TierAuditEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, TierAuditEntry{
			ActorEmail: r.ActorEmail,
			ActorUUID:  r.ActorUUID,
			Source:     r.Source,
			OldTier:    r.OldTier,
			NewTier:    r.NewTier,
			CreatedAt:  r.CreatedAt,
		})
	}
	return out, nil
}
