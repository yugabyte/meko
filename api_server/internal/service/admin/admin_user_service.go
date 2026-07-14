// Package admin: admin_user_service handles admin-initiated user
// mutations (currently tier overrides) for the meko_admin console.
package admin

import (
	"context"
	"errors"
)

// ErrInvalidTier is returned when a tier update names a value outside the
// meko_system.account_type enum; the handler maps it to 400.
var ErrInvalidTier = errors.New("invalid tier")

// validTiers are the only tiers the admin switch may set: Free <-> Pro.
var validTiers = map[string]bool{"free": true, "pro": true}

// UpdateUserTier directly overrides a user's user_type for admin/comp
// grants. Returns ErrInvalidTier for a non-free/pro tier, ErrUserNotFound
// when the user does not exist.
func (s *AdminMetricsService) UpdateUserTier(ctx context.Context, userID, tier string) error {
	if !validTiers[tier] {
		return ErrInvalidTier
	}
	updated, err := s.repo.UpdateUserTier(ctx, userID, tier)
	if err != nil {
		return err
	}
	if !updated {
		return ErrUserNotFound
	}
	return nil
}
