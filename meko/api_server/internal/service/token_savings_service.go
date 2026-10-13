package service

import (
	"context"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// TokenSavingsService wraps TokenSavingsRepo. The account-level view resolves
// the caller's accessible datapack set (owned + shared/invited, the same set
// GET /api/v1/datapacks returns) inside the repo's query.
type TokenSavingsService struct {
	repo *repository.TokenSavingsRepo
}

// NewTokenSavingsService creates a new TokenSavingsService.
func NewTokenSavingsService(repo *repository.TokenSavingsRepo) *TokenSavingsService {
	return &TokenSavingsService{repo: repo}
}

// GetForDatapack returns the token-savings summary for one datapack. The
// caller must already have verified datapack access (auth.RequireDatapackAccess
// runs before the handler reaches this).
func (s *TokenSavingsService) GetForDatapack(ctx context.Context, datapackID string) (*models.TokenSavingsSummary, error) {
	return s.repo.GetSummary(ctx, []string{datapackID})
}

// accountTopDatapacks caps by_datapack on the account view.
const accountTopDatapacks = 100

// GetForAccount returns a rollup across every datapack the account can
// access (owned + shared). by_datapack lists the datapacks with savings, most
// first, capped at accountTopDatapacks.
func (s *TokenSavingsService) GetForAccount(ctx context.Context, accountID string) (*models.TokenSavingsSummary, error) {
	return s.repo.GetAccountSummary(ctx, accountID, accountTopDatapacks)
}
