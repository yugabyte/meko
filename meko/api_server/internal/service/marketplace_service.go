// Package service — AWS Marketplace integration.
//
// Meko sells via AWS Marketplace as a SaaS Contract product. This
// service wraps two AWS APIs:
//
//   - marketplacemetering.ResolveCustomer — trades the opaque token
//     AWS posts to /marketplace/register for a stable CustomerIdentifier
//   - AWS account id + product code.
//   - marketplaceentitlementservice.GetEntitlements — reads which
//     dimension (tier) the buyer is entitled to right now.
//
// Called from two places:
//   - MarketplaceHandler.Register: first-touch fulfillment, upserts a
//     new row and sets the user's tier.
//   - MarketplaceHandler.SNS: entitlement-change notification handler,
//     re-runs GetEntitlements and updates the row.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsCfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/marketplaceentitlementservice"
	entTypes "github.com/aws/aws-sdk-go-v2/service/marketplaceentitlementservice/types"
	"github.com/aws/aws-sdk-go-v2/service/marketplacemetering"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"meko/api_server/internal/repository"
)

// MarketplaceConfig carries the deploy-time config the service needs.
// All fields are required for Enabled() to return true.
type MarketplaceConfig struct {
	// AWS-assigned ProductCode from the listing overview. Set via
	// AWS_MARKETPLACE_PRODUCT_CODE. Both ResolveCustomer and
	// GetEntitlements only accept tokens/queries scoped to this
	// product; a mismatch silently returns zero entitlements.
	ProductCode string

	// AWS SDK region. Marketplace APIs are only served from us-east-1
	// today, regardless of where your app runs. Leave empty to default.
	Region string

	// RoleARN (optional) — when the AWS account that owns the
	// Marketplace product listing (the "seller" account) is DIFFERENT
	// from the account api_server runs in (the "app" account), point
	// this at a cross-account role in the seller account. api_server
	// will STS:AssumeRole into that role before making any
	// ResolveCustomer / GetEntitlements call, so those calls carry the
	// seller's authority (which is what AWS Marketplace checks for
	// authorization).
	//
	// Set via AWS_MARKETPLACE_ROLE_ARN. Format:
	//   arn:aws:iam::<seller-account-id>:role/<role-name>
	//
	// Leave empty for single-account deployments where the app account
	// is also the seller — the SDK's ambient credentials work
	// unmodified in that case.
	RoleARN string
}

// Marketplace-side status values we care about. We stash them as
// strings on the DB row without leaning on the SDK types anywhere
// downstream.
const (
	MarketplaceStatusEntitled = "ENTITLED"
	MarketplaceStatusExpired  = "EXPIRED"
)

// MarketplaceService owns the AWS SDK clients and the DB repo. Safe to
// share across goroutines — both AWS clients and the *sql.DB inside
// the repo are stateless in practice.
type MarketplaceService struct {
	cfg            MarketplaceConfig
	repo           *repository.MarketplaceRepo
	billingRepo    *repository.BillingRepo
	meteringClient *marketplacemetering.Client
	entitlementCli *marketplaceentitlementservice.Client
}

// NewMarketplaceService builds the service. Returns (nil, nil) when
// cfg.ProductCode is empty — that signals the operator hasn't finished
// the AWS listing yet. Handlers guard on Enabled() so the routes 503
// cleanly in that state.
func NewMarketplaceService(
	ctx context.Context,
	cfg MarketplaceConfig,
	repo *repository.MarketplaceRepo,
	billingRepo *repository.BillingRepo,
) (*MarketplaceService, error) {
	if cfg.ProductCode == "" {
		slog.Warn("marketplace: disabled (AWS_MARKETPLACE_PRODUCT_CODE not set)")
		return nil, nil
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	awsConf, err := awsCfg.LoadDefaultConfig(ctx, awsCfg.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("marketplace: load aws config: %w", err)
	}

	// Cross-account: when RoleARN is set, wrap the ambient config with
	// an STS AssumeRole credentials provider. Marketplace API calls
	// (ResolveCustomer / GetEntitlements) then run under the seller
	// account's authority instead of the app pod's. All OTHER AWS SDK
	// callers in api_server keep using ambient creds -- we only rewrap
	// the config the marketplace clients are constructed from.
	marketplaceCfg := awsConf
	if cfg.RoleARN != "" {
		stsClient := sts.NewFromConfig(awsConf)
		provider := stscreds.NewAssumeRoleProvider(stsClient, cfg.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			// Human-readable name that shows up in CloudTrail on the
			// seller account, making cross-account calls easy to trace.
			o.RoleSessionName = "meko-api-server-marketplace"
		})
		marketplaceCfg = awsConf.Copy()
		marketplaceCfg.Credentials = aws.NewCredentialsCache(provider)
		slog.Info("marketplace: cross-account role configured", "role_arn", cfg.RoleARN)
	}

	svc := &MarketplaceService{
		cfg:            cfg,
		repo:           repo,
		billingRepo:    billingRepo,
		meteringClient: marketplacemetering.NewFromConfig(marketplaceCfg),
		entitlementCli: marketplaceentitlementservice.NewFromConfig(marketplaceCfg),
	}
	slog.Info("marketplace: initialized",
		"product_code", cfg.ProductCode,
		"region", region,
		"cross_account", cfg.RoleARN != "",
	)
	return svc, nil
}

// Enabled reports whether the service is wired up. Handlers check this
// and return 503 when false so a fresh install without an AWS listing
// yet doesn't crash on route hits.
func (s *MarketplaceService) Enabled() bool {
	return s != nil && s.cfg.ProductCode != ""
}

// ResolveCustomerResult carries the identifying triple AWS returns
// once the redirect token has been exchanged. We store all three so
// the SNS handler can reverse-lookup without hitting AWS again.
type ResolveCustomerResult struct {
	CustomerIdentifier string
	CustomerAWSAccount string
	ProductCode        string
}

// ResolveCustomer exchanges the `x-amzn-marketplace-token` that AWS
// form-posts to our fulfillment URL for a stable customer identifier.
// The token is single-use — call this exactly once per redirect and
// persist the resulting identifier immediately.
func (s *MarketplaceService) ResolveCustomer(
	ctx context.Context, token string,
) (*ResolveCustomerResult, error) {
	if !s.Enabled() {
		return nil, errors.New("marketplace: service disabled")
	}
	out, err := s.meteringClient.ResolveCustomer(ctx,
		&marketplacemetering.ResolveCustomerInput{RegistrationToken: &token})
	if err != nil {
		return nil, fmt.Errorf("marketplace: resolve customer: %w", err)
	}
	res := &ResolveCustomerResult{}
	if out.CustomerIdentifier != nil {
		res.CustomerIdentifier = *out.CustomerIdentifier
	}
	if out.CustomerAWSAccountId != nil {
		res.CustomerAWSAccount = *out.CustomerAWSAccountId
	}
	if out.ProductCode != nil {
		res.ProductCode = *out.ProductCode
	}
	if res.ProductCode != "" && res.ProductCode != s.cfg.ProductCode {
		return nil, fmt.Errorf(
			"marketplace: token product %q does not match configured %q",
			res.ProductCode, s.cfg.ProductCode)
	}
	return res, nil
}

// EntitlementSnapshot is the flattened result of GetEntitlements —
// enough to write a marketplace_entitlements row.
type EntitlementSnapshot struct {
	// Dimension is the AWS-side name for the plan tier
	// (starter / growth / scale / enterprise). AWS lets sellers pick
	// any strings for their dimensions; we map to Meko's account_type
	// enum in `PlanIDFromDimension` — keep the two lists in sync.
	Dimension string
	Status    string
	// ExpiresAt may be zero for perpetual entitlements.
	ExpiresAt time.Time
}

// GetEntitlement returns the single entitlement AWS reports for this
// customer, or nil if the customer has no active entitlement. On a
// paid SaaS Contract each customer has exactly one active dimension;
// on cancellation the entitlement transitions to EXPIRED and stays
// visible until the contract term ends.
func (s *MarketplaceService) GetEntitlement(
	ctx context.Context, awsCustomerID string,
) (*EntitlementSnapshot, error) {
	if !s.Enabled() {
		return nil, errors.New("marketplace: service disabled")
	}
	filter := map[string][]string{
		string(entTypes.GetEntitlementFilterNameCustomerIdentifier): {awsCustomerID},
	}
	out, err := s.entitlementCli.GetEntitlements(ctx,
		&marketplaceentitlementservice.GetEntitlementsInput{
			ProductCode: &s.cfg.ProductCode,
			Filter:      castFilter(filter),
		})
	if err != nil {
		return nil, fmt.Errorf("marketplace: get entitlements: %w", err)
	}
	if len(out.Entitlements) == 0 {
		return nil, nil
	}
	// SaaS Contracts allow at most one active entitlement per
	// (customer, product); if we ever see multiples we pick the one
	// with the furthest expiry (the most recent upgrade). A nil
	// ExpirationDate signals a perpetual / no-end-date entitlement
	// (see snap.ExpiresAt handling below) -- treat it as MAX so it
	// wins over any dated entitlement, including a not-yet-expired
	// one that might otherwise look "further".
	best := out.Entitlements[0]
	for _, e := range out.Entitlements[1:] {
		// Perpetual (nil) is already the furthest future; nothing beats it.
		if best.ExpirationDate == nil {
			break
		}
		// A perpetual candidate beats any dated best.
		if e.ExpirationDate == nil {
			best = e
			break
		}
		// Both dated -- pick the one with the later expiry.
		if e.ExpirationDate.After(*best.ExpirationDate) {
			best = e
		}
	}
	snap := &EntitlementSnapshot{
		Status: MarketplaceStatusEntitled,
	}
	if best.Dimension != nil {
		snap.Dimension = *best.Dimension
	}
	if best.ExpirationDate != nil {
		snap.ExpiresAt = *best.ExpirationDate
		if best.ExpirationDate.Before(time.Now()) {
			snap.Status = MarketplaceStatusExpired
		}
	}
	return snap, nil
}

// PlanIDFromDimension converts an AWS Marketplace Dimension string to
// the meko_system.account_type value we store on the row. Unknown
// dimensions collapse to "" so the caller can 400 instead of writing
// garbage.
//
// Maps AWS Marketplace dimension "API name" → meko user_type. Only
// "standard" is expected today (the self-serve $25/mo tier); enterprise
// is sold off-marketplace so never reaches this path. Whatever dim
// string is set here MUST match the API name entered when defining the
// dimension in the Marketplace Management Portal — AWS forces the
// choice at product-create time and it CANNOT be renamed after publish.
//
// Case-insensitive so a portal-side "Standard" typo doesn't silently
// drop entitlements. Unknown dims return "" → handler surfaces
// UNKNOWN_DIMENSION and refuses the register.
func PlanIDFromDimension(dim string) string {
	switch strings.ToLower(strings.TrimSpace(dim)) {
	case "standard":
		return "standard"
	default:
		return ""
	}
}

// castFilter is a tiny adapter — the SDK's Filter map is keyed by
// GetEntitlementFilterName (string-typed) but our call site uses
// plain strings for readability.
func castFilter(m map[string][]string) map[string][]string {
	// Placeholder for future validation; leaving as pass-through so
	// the call site reads naturally.
	return m
}
