package service

import (
	"context"
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// TokenSavingsService wraps TokenSavingsRepo. For the account-level view it
// first resolves the caller's accessible datapack set via DatapackRepo.List
// (owned + shared/invited — the same set GET /api/v1/datapacks returns).
type TokenSavingsService struct {
	repo   *repository.TokenSavingsRepo
	dpRepo *repository.DatapackRepo
}

// NewTokenSavingsService creates a new TokenSavingsService.
func NewTokenSavingsService(repo *repository.TokenSavingsRepo, dpRepo *repository.DatapackRepo) *TokenSavingsService {
	return &TokenSavingsService{repo: repo, dpRepo: dpRepo}
}

// GetForDatapack returns the token-savings summary for one datapack. The
// caller must already have verified datapack access (auth.RequireDatapackAccess
// runs before the handler reaches this).
func (s *TokenSavingsService) GetForDatapack(ctx context.Context, datapackID string) (*models.TokenSavingsSummary, error) {
	return s.repo.GetSummary(ctx, []string{datapackID})
}

// GetForAccount returns a rollup across every datapack the account can
// access (owned + shared), broken down by datapack.
func (s *TokenSavingsService) GetForAccount(ctx context.Context, accountID string) (*models.TokenSavingsSummary, error) {
	datapacks, err := s.dpRepo.List(accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list datapacks for account: %w", err)
	}
	ids := make([]string, len(datapacks))
	for i, dp := range datapacks {
		ids[i] = dp.ID
	}
	return s.repo.GetSummary(ctx, ids)
}
