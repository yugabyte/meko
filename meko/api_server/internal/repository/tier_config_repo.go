package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// TierConfig holds the container images and replica counts for a
// specific account tier.
type TierConfig struct {
	Tier        string
	MCPImage    string
	RAGImage    string
	MCPReplicas int32
	RAGReplicas   int32
}

// TierConfigRepo encapsulates database operations for the tier_config
// table.
type TierConfigRepo struct {
	db *sql.DB
}

// NewTierConfigRepo creates a new TierConfigRepo.
func NewTierConfigRepo(db *sql.DB) *TierConfigRepo {
	return &TierConfigRepo{db: db}
}

// GetByTier returns the configuration for the given account tier.
func (r *TierConfigRepo) GetByTier(ctx context.Context, tier string) (*TierConfig, error) {
	slog.Debug("fetching tier config", "tier", tier)

	var tc TierConfig
	err := r.db.QueryRowContext(ctx,
		`SELECT tier, mcp_image, rag_image,
		        mcp_replicas, rag_replicas
		 FROM meko_system.tier_config
		 WHERE tier = $1`,
		tier,
	).Scan(
		&tc.Tier, &tc.MCPImage, &tc.RAGImage,
		&tc.MCPReplicas, &tc.RAGReplicas,
	)
	if err == sql.ErrNoRows {
		slog.Error("tier config not found", "tier", tier)
		return nil, fmt.Errorf("tier config not found for tier %q", tier)
	}
	if err != nil {
		slog.Error("failed to fetch tier config", "tier", tier, "error", err)
		return nil, fmt.Errorf("failed to fetch tier config: %w", err)
	}

	return &tc, nil
}
