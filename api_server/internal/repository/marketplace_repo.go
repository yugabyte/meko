package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MarketplaceRepo owns meko_system.marketplace_entitlements — the
// mapping between a Meko user and their AWS Marketplace subscription.
//
// Written by MarketplaceService on the /marketplace/register fulfillment
// call and on incoming SNS entitlement-change notifications. The
// consumer side (tier lookups for gating) reads via GetByUserID and
// falls back to Stripe / free if no row exists.
type MarketplaceRepo struct {
	db *sql.DB
}

func NewMarketplaceRepo(db *sql.DB) *MarketplaceRepo {
	return &MarketplaceRepo{db: db}
}

// MarketplaceEntitlement mirrors the table row. `ExpiresAt` is nil for
// perpetual entitlements (rare on SaaS Contracts, so treat nil as
// "no known expiry" not "expired").
type MarketplaceEntitlement struct {
	UserID          string
	AWSCustomerID   string
	AWSAccountID    string
	ProductCode     string
	PlanID          string
	Status          string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Upsert writes the entitlement, keyed on user_id. Called at register
// time (first subscription) and on every SNS re-resolve.
func (r *MarketplaceRepo) Upsert(ctx context.Context, e MarketplaceEntitlement) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.marketplace_entitlements
			(user_id, aws_customer_id, aws_account_id, product_code,
			 plan_id, status, expires_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5::meko_system.account_type, $6, $7, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET
			aws_customer_id = EXCLUDED.aws_customer_id,
			aws_account_id  = EXCLUDED.aws_account_id,
			product_code    = EXCLUDED.product_code,
			plan_id         = EXCLUDED.plan_id,
			status          = EXCLUDED.status,
			expires_at      = EXCLUDED.expires_at,
			updated_at      = NOW()`,
		e.UserID, e.AWSCustomerID, nullString(e.AWSAccountID), e.ProductCode,
		e.PlanID, e.Status, nullTime(e.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("marketplace: upsert entitlement: %w", err)
	}
	return nil
}

// GetByUserID returns (nil, nil) when the user has no marketplace
// subscription — the caller then falls back to Stripe / free tier.
func (r *MarketplaceRepo) GetByUserID(
	ctx context.Context, userID string,
) (*MarketplaceEntitlement, error) {
	e, err := r.scanOne(ctx,
		`SELECT user_id, aws_customer_id, COALESCE(aws_account_id, ''),
			product_code, plan_id::text, status, expires_at,
			created_at, updated_at
		 FROM meko_system.marketplace_entitlements
		 WHERE user_id = $1`, userID)
	return e, err
}

// GetByAWSCustomerID drives the SNS handler: the SNS payload includes
// only the AWS-side identifier, so we look up the mapped Meko user
// here before re-resolving the entitlement.
func (r *MarketplaceRepo) GetByAWSCustomerID(
	ctx context.Context, awsCustomerID string,
) (*MarketplaceEntitlement, error) {
	return r.scanOne(ctx,
		`SELECT user_id, aws_customer_id, COALESCE(aws_account_id, ''),
			product_code, plan_id::text, status, expires_at,
			created_at, updated_at
		 FROM meko_system.marketplace_entitlements
		 WHERE aws_customer_id = $1`, awsCustomerID)
}

func (r *MarketplaceRepo) scanOne(
	ctx context.Context, query string, args ...any,
) (*MarketplaceEntitlement, error) {
	var (
		e         MarketplaceEntitlement
		expiresAt sql.NullTime
	)
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&e.UserID, &e.AWSCustomerID, &e.AWSAccountID, &e.ProductCode,
		&e.PlanID, &e.Status, &expiresAt, &e.CreatedAt, &e.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace: scan entitlement: %w", err)
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		e.ExpiresAt = &t
	}
	return &e, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
