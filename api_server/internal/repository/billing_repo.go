package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// BillingRepo owns the meko_system.stripe_customers,
// meko_system.billing_subscriptions, and meko_system.billing_webhook_events
// tables, plus the user_type column on meko_system.users when it gets
// written from a subscription state change.
//
// The webhook handler is the only path that calls UpsertSubscription /
// SetUserType; the customer-lookup + ensure path is used by the
// checkout / portal handlers.
type BillingRepo struct {
	db *sql.DB
}

// NewBillingRepo creates a new BillingRepo against the meko_system database.
func NewBillingRepo(db *sql.DB) *BillingRepo {
	return &BillingRepo{db: db}
}

// StripeCustomer is the row shape returned from GetCustomerByUserID /
// GetCustomerByStripeID.
type StripeCustomer struct {
	UserID           string
	StripeCustomerID string
	Email            string
}

// GetCustomerByUserID returns (nil, nil) when no Stripe customer exists
// yet for that user -- we lazy-create on the first Checkout.
func (r *BillingRepo) GetCustomerByUserID(
	ctx context.Context, userID string,
) (*StripeCustomer, error) {
	var c StripeCustomer
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, stripe_customer_id, email
		 FROM meko_system.stripe_customers
		 WHERE user_id = $1`,
		userID,
	).Scan(&c.UserID, &c.StripeCustomerID, &c.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lookup customer by user_id: %w", err)
	}
	return &c, nil
}

// GetSubscriptionByUserID returns the active subscription row or
// (nil, nil) when the user is on the Free tier (no row written).
// Used by the BillingService.GetBillingSummary path to populate the
// "Renews" pill on the current-plan card.
func (r *BillingRepo) GetSubscriptionByUserID(
	ctx context.Context, userID string,
) (*SubscriptionRecord, error) {
	var s SubscriptionRecord
	var startNT, endNT sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, stripe_customer_id, stripe_subscription_id,
				stripe_price_id, status, plan_id::text,
				current_period_start, current_period_end, cancel_at_period_end
		 FROM meko_system.billing_subscriptions
		 WHERE user_id = $1`,
		userID,
	).Scan(
		&s.UserID, &s.StripeCustomerID, &s.StripeSubscriptionID,
		&s.StripePriceID, &s.Status, &s.PlanID,
		&startNT, &endNT, &s.CancelAtPeriodEnd,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lookup subscription: %w", err)
	}
	if startNT.Valid {
		s.CurrentPeriodStart = &startNT.Time
	}
	if endNT.Valid {
		s.CurrentPeriodEnd = &endNT.Time
	}
	return &s, nil
}

// GetCustomerByStripeID is the webhook-side path: a Stripe event names
// the customer; we need the Meko user_id to update user_type.
func (r *BillingRepo) GetCustomerByStripeID(
	ctx context.Context, stripeCustomerID string,
) (*StripeCustomer, error) {
	var c StripeCustomer
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, stripe_customer_id, email
		 FROM meko_system.stripe_customers
		 WHERE stripe_customer_id = $1`,
		stripeCustomerID,
	).Scan(&c.UserID, &c.StripeCustomerID, &c.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lookup customer by stripe id: %w", err)
	}
	return &c, nil
}

// UpsertCustomer creates or refreshes the Meko<->Stripe id mapping.
// Idempotent so the checkout path can call it unconditionally on every
// session-creation request (cheap upsert vs. an extra SELECT each time).
func (r *BillingRepo) UpsertCustomer(
	ctx context.Context, userID, stripeCustomerID, email string,
) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.stripe_customers
			(user_id, stripe_customer_id, email)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO UPDATE
			SET stripe_customer_id = EXCLUDED.stripe_customer_id,
				email              = EXCLUDED.email,
				updated_at         = NOW()`,
		userID, stripeCustomerID, email,
	)
	if err != nil {
		return fmt.Errorf("billing: upsert stripe customer: %w", err)
	}
	return nil
}

// SubscriptionRecord is the slim subscription state the BillingService
// keeps in sync with Stripe. Mirrors meko_system.billing_subscriptions.
type SubscriptionRecord struct {
	UserID               string
	StripeCustomerID     string
	StripeSubscriptionID string
	StripePriceID        string
	Status               string
	PlanID               string // matches meko_system.account_type
	CurrentPeriodStart   *time.Time
	CurrentPeriodEnd     *time.Time
	CancelAtPeriodEnd    bool
}

// UpsertSubscription writes the subscription row AND advances the
// user's user_type in the same transaction so the UI never observes a
// state where the sub says "active growth" but the user row still says
// "free". Both writes go through one tx; on failure we roll back and
// the webhook handler will return non-2xx so Stripe retries.
func (r *BillingRepo) UpsertSubscription(
	ctx context.Context, sub SubscriptionRecord,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("billing: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			slog.Warn("billing upsert rollback error", "user_id", sub.UserID, "error", rbErr)
		}
	}()

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO meko_system.billing_subscriptions
			(user_id, stripe_customer_id, stripe_subscription_id,
			 stripe_price_id, status, plan_id,
			 current_period_start, current_period_end, cancel_at_period_end)
		 VALUES ($1, $2, $3, $4, $5, $6::meko_system.account_type, $7, $8, $9)
		 ON CONFLICT (user_id) DO UPDATE
			SET stripe_customer_id     = EXCLUDED.stripe_customer_id,
				stripe_subscription_id = EXCLUDED.stripe_subscription_id,
				stripe_price_id        = EXCLUDED.stripe_price_id,
				status                 = EXCLUDED.status,
				plan_id                = EXCLUDED.plan_id,
				current_period_start   = EXCLUDED.current_period_start,
				current_period_end     = EXCLUDED.current_period_end,
				cancel_at_period_end   = EXCLUDED.cancel_at_period_end,
				updated_at             = NOW()`,
		sub.UserID, sub.StripeCustomerID, sub.StripeSubscriptionID,
		sub.StripePriceID, sub.Status, sub.PlanID,
		sub.CurrentPeriodStart, sub.CurrentPeriodEnd, sub.CancelAtPeriodEnd,
	); err != nil {
		return fmt.Errorf("billing: upsert subscription: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET user_type = $1::meko_system.account_type,
			 updated_at = NOW()
		 WHERE user_id = $2`,
		sub.PlanID, sub.UserID,
	); err != nil {
		return fmt.Errorf("billing: update user_type: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("billing: commit subscription tx: %w", err)
	}
	return nil
}

// DowngradeToFree handles the canceled / unpaid / paused branch: drop
// the subscription row and reset the user back to the free tier in one
// tx. The row is removed (not just status-flipped) so the
// stripe_subscription_id unique index stays usable when the user
// re-subscribes later with a fresh sub id.
func (r *BillingRepo) DowngradeToFree(ctx context.Context, userID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("billing: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			slog.Warn("billing downgrade rollback error", "user_id", userID, "error", rbErr)
		}
	}()

	if _, err = tx.ExecContext(ctx,
		`DELETE FROM meko_system.billing_subscriptions WHERE user_id = $1`,
		userID,
	); err != nil {
		return fmt.Errorf("billing: delete subscription: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET user_type = 'free'::meko_system.account_type,
			 updated_at = NOW()
		 WHERE user_id = $1`,
		userID,
	); err != nil {
		return fmt.Errorf("billing: reset user_type: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("billing: commit downgrade tx: %w", err)
	}
	return nil
}

// MarkEventProcessed records a webhook event id so a retried delivery
// short-circuits in the handler. Returns true when this is the first
// time we've seen the event id, false when it's a duplicate -- the
// handler uses the return value to decide whether to skip downstream
// state changes entirely.
func (r *BillingRepo) MarkEventProcessed(
	ctx context.Context, eventID, eventType string,
) (firstTime bool, err error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.billing_webhook_events (event_id, event_type)
		 VALUES ($1, $2)
		 ON CONFLICT (event_id) DO NOTHING`,
		eventID, eventType,
	)
	if err != nil {
		return false, fmt.Errorf("billing: mark webhook event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("billing: webhook event rows-affected: %w", err)
	}
	return n > 0, nil
}
