package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"meko/api_server/internal/repository"
)

// ErrInvalidRedirectBase is returned when neither the request-derived
// base URL nor the configured AppURL is a usable absolute URL.
// Sending Stripe a relative path would 400 with an opaque error;
// failing fast here surfaces the misconfiguration in our own logs
// the first time a user attempts to upgrade.
var ErrInvalidRedirectBase = errors.New("billing: no absolute base URL configured for Stripe redirects")

// validAbsoluteBaseURL reports whether the given string parses as an
// absolute URL (scheme + host) suitable for Stripe redirects.
func validAbsoluteBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// resolveRedirectBase picks the request-derived base when it's an
// absolute URL, otherwise falls back to cfg.AppURL. Returns
// ErrInvalidRedirectBase when neither is usable
func resolveRedirectBase(requestBase, appURL string) (string, error) {
	if validAbsoluteBaseURL(requestBase) {
		return strings.TrimRight(requestBase, "/"), nil
	}
	if validAbsoluteBaseURL(appURL) {
		return strings.TrimRight(appURL, "/"), nil
	}
	return "", ErrInvalidRedirectBase
}

// BillingConfig collects the Stripe wiring the BillingService needs.
// Populated once at server start from CLI flags / env; the service
// itself holds it by value.
type BillingConfig struct {
	StripeSecretKey     string
	StripeWebhookSecret string
	// Stripe price IDs per Meko plan tier. Enterprise has no price --
	// those deals are billed off-platform.
	PriceIDStarter string
	PriceIDGrowth  string
	PriceIDScale   string
	// AppURL is used as the base for the Checkout success / cancel
	// redirects and the Customer Portal return URL.
	AppURL string
}

// BillingService coordinates the Stripe REST client and the
// billing repository. The Echo handler stays thin -- request shape +
// auth context only -- and the webhook processing path lives here.
type BillingService struct {
	cfg    BillingConfig
	stripe *StripeClient
	repo   *repository.BillingRepo
}

// NewBillingService wires the BillingService. The returned value is
// always non-nil so callers can compose against it; check Enabled()
// before exposing the routes.
func NewBillingService(cfg BillingConfig, repo *repository.BillingRepo) *BillingService {
	return &BillingService{
		cfg:    cfg,
		stripe: NewStripeClient(cfg.StripeSecretKey, cfg.StripeWebhookSecret),
		repo:   repo,
	}
}

// Enabled reports whether the billing routes should be registered.
// When the operator hasn't configured Stripe keys we leave the routes
// off entirely rather than respond 503 on every call -- a missing
// route surfaces the misconfiguration loudly the first time the UI
// attempts an upgrade.
func (s *BillingService) Enabled() bool {
	return s != nil && s.stripe.Enabled() && s.repo != nil &&
		(s.cfg.PriceIDStarter != "" ||
			s.cfg.PriceIDGrowth != "" ||
			s.cfg.PriceIDScale != "")
}

// AppURL exposes the configured base URL for the handler's redirect
// allowlist check. Used to validate that Origin/Referer values lifted
// off the request match a host we actually serve, preventing open
// redirects via Stripe Checkout / Portal.
func (s *BillingService) AppURL() string {
	if s == nil {
		return ""
	}
	return s.cfg.AppURL
}

// ErrUnknownPlan is returned by CreateCheckoutSession when the caller
// asks for a tier with no configured Stripe price id (typically
// "free" or "enterprise", neither of which goes through Checkout).
var ErrUnknownPlan = errors.New("billing: no stripe price configured for that plan")

// ErrCustomerNotFound is returned by CreatePortalSession when the user
// has never been through Checkout -- they have no Stripe customer yet
// and there is nothing to manage. UI should surface "upgrade first".
var ErrCustomerNotFound = errors.New("billing: no stripe customer for user")

// ErrAlreadySubscribed is returned by CreateCheckoutSession when the
// user already has an active local subscription row. Subsequent plan
// changes must go through the Customer Portal so Stripe can prorate
// and avoid creating a duplicate parallel subscription against the
// same customer (Stripe itself happily allows N concurrent subs --
// the no-double-subscribe rule is ours to enforce).
var ErrAlreadySubscribed = errors.New("billing: user already has an active subscription")

// CheckoutInput is the BillingService-level shape of the
// /billing/checkout-session request. The handler builds it from JWT
// context + JSON body.
type CheckoutInput struct {
	UserID string
	Email  string
	PlanID string // "starter" | "growth" | "scale"
	// BaseURL overrides the configured AppURL for the success / cancel
	// redirects when set. The handler populates this from the request's
	// Origin header so local-dev sessions land back on localhost and
	// prod sessions land on whichever hostname the UI is served from.
	// Empty -> fall back to cfg.AppURL.
	BaseURL string
}

// CreatecheckoutSessionOutput surfaces just the URL the UI redirects
// to. We don't return the Stripe session id today; the
// checkout.session.completed webhook is the canonical signal.
type CreateCheckoutSessionOutput struct {
	URL string
}

// CreateCheckoutSession resolves the Stripe customer (creating the
// mapping row lazily if needed) and asks Stripe for a Checkout URL.
//
// Returns ErrAlreadySubscribed when the user has an active local
// subscription row -- Checkout would create a *second* parallel sub
// (Stripe permits it, double-billing the customer). Plan changes for
// existing subscribers must go through the Customer Portal so Stripe
// can prorate and swap the price on the same sub.
func (s *BillingService) CreateCheckoutSession(
	ctx context.Context, in CheckoutInput,
) (*CreateCheckoutSessionOutput, error) {
	priceID := s.priceIDFor(in.PlanID)
	if priceID == "" {
		return nil, ErrUnknownPlan
	}

	// Guard against double-subscribe. We treat any non-terminal sub
	// status (the same set as isPaidStatus) as "blocking" -- canceled
	// rows are deleted from billing_subscriptions so a fresh checkout
	// after cancel is allowed naturally.
	if active, err := s.repo.GetSubscriptionByUserID(ctx, in.UserID); err != nil {
		return nil, err
	} else if active != nil {
		return nil, ErrAlreadySubscribed
	}

	existing, err := s.repo.GetCustomerByUserID(ctx, in.UserID)
	if err != nil {
		return nil, err
	}

	base, err := resolveRedirectBase(in.BaseURL, s.cfg.AppURL)
	if err != nil {
		return nil, err
	}
	stripeIn := CheckoutSessionInput{
		PriceID:           priceID,
		SuccessURL:        base + "/billing?checkout=success",
		CancelURL:         base + "/billing?checkout=cancelled",
		ClientReferenceID: in.UserID,
	}
	if existing != nil {
		stripeIn.CustomerID = existing.StripeCustomerID
	} else {
		stripeIn.CustomerEmail = in.Email
	}

	sess, err := s.stripe.CreateCheckoutSession(ctx, stripeIn)
	if err != nil {
		return nil, err
	}
	return &CreateCheckoutSessionOutput{URL: sess.URL}, nil
}

// CreatePortalSession asks Stripe for a one-time Customer Portal URL.
// Returns ErrCustomerNotFound when the user has never paid -- the UI
// can render "go upgrade first" instead of opening a dead link.
//
// baseURL overrides cfg.AppURL when non-empty (same pattern as
// CreateCheckoutSession: the handler passes the request's Origin so
// local-dev sessions land back on localhost).
func (s *BillingService) CreatePortalSession(
	ctx context.Context, userID, baseURL string,
) (string, error) {
	cust, err := s.repo.GetCustomerByUserID(ctx, userID)
	if err != nil {
		return "", err
	}
	if cust == nil {
		return "", ErrCustomerNotFound
	}
	base, err := resolveRedirectBase(baseURL, s.cfg.AppURL)
	if err != nil {
		return "", err
	}
	sess, err := s.stripe.CreatePortalSession(ctx,
		cust.StripeCustomerID, base+"/billing")
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// BillingSummary is the BillingService-level shape returned to the
// UI's CurrentPlanCard. All fields are optional -- a Free user has
// no subscription row and no card on file, so the FE just renders
// em-dashes in their place.
type BillingSummary struct {
	PlanID            string     // matches users.user_type
	Status            string     // "active" | "trialing" | "" when no sub
	CurrentPeriodEnd  *time.Time // "Renews May 28, 2026"
	CancelAtPeriodEnd bool
	CardBrand         string // "visa" | "mastercard" | ...
	CardLast4         string // "4242"
}

// GetBillingSummary collects the current-plan card data from local
// state (subscription) + Stripe (card on file). Returns a zero-value
// summary (all fields blank) when the user has no subscription --
// the FE then renders the Free-tier placeholders.
//
// Errors from the Stripe card fetch are swallowed and logged: missing
// card info is a soft failure (still useful to show the renew date),
// while a hard failure on the local DB read aborts as usual.
func (s *BillingService) GetBillingSummary(
	ctx context.Context, userID string,
) (*BillingSummary, error) {
	sub, err := s.repo.GetSubscriptionByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return &BillingSummary{}, nil
	}

	out := &BillingSummary{
		PlanID:            sub.PlanID,
		Status:            sub.Status,
		CurrentPeriodEnd:  sub.CurrentPeriodEnd,
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
	}

	// Stripe round-trip for the card. Best-effort: if it fails, we
	// still return the local fields so the UI can show Renews even
	// when Card on file blanks.
	card, cardErr := s.stripe.GetDefaultCard(ctx, sub.StripeCustomerID)
	if cardErr != nil {
		slog.Warn("billing: fetch default card failed; returning summary without card",
			"user_id", userID, "error", cardErr)
	} else if card != nil {
		out.CardBrand = card.Brand
		out.CardLast4 = card.Last4
	}
	return out, nil
}

// HandleWebhook verifies the Stripe signature, dedupes via the
// webhook_events table, and routes to the per-event handler. Returns
// nil on both "applied" and "ignored as duplicate" -- both mean we
// should respond 200 so Stripe stops retrying.
func (s *BillingService) HandleWebhook(
	ctx context.Context, signatureHeader string, body []byte,
) error {
	evt, err := s.stripe.VerifyWebhookSignature(signatureHeader, body, time.Now())
	if err != nil {
		return err
	}

	first, err := s.repo.MarkEventProcessed(ctx, evt.ID, evt.Type)
	if err != nil {
		return err
	}
	if !first {
		slog.Debug("billing: duplicate webhook ignored",
			"event_id", evt.ID, "event_type", evt.Type)
		return nil
	}

	switch evt.Type {
	case "checkout.session.completed":
		// Populates stripe_customers with (user_id, customer_id, email)
		// so the next "Manage payment" click can resolve the customer
		// without a Stripe API round-trip. Subscription events that
		// arrive a moment later carry the same metadata so we don't
		// strictly need this row -- but the portal-session path does.
		return s.applyCheckoutSessionCompleted(ctx, evt)

	case "customer.subscription.created",
		"customer.subscription.updated":
		return s.applySubscriptionEvent(ctx, evt)

	case "customer.subscription.deleted":
		return s.applySubscriptionDeleted(ctx, evt)

	case "invoice.paid":
		// invoice.paid carries the full sub status indirectly via the
		// related subscription update event, which Stripe also sends.
		// Logging here keeps an audit trail without double-applying.
		slog.Info("billing: invoice.paid", "event_id", evt.ID)
		return nil

	case "invoice.payment_failed":
		slog.Warn("billing: invoice.payment_failed",
			"event_id", evt.ID)
		// We deliberately don't downgrade here: Stripe's smart retries
		// will recover most failures, and a single missed payment
		// shouldn't yank the user off their plan. The subsequent
		// customer.subscription.updated event flips status to
		// past_due / unpaid / canceled and we react to that.
		return nil

	default:
		slog.Debug("billing: webhook event type ignored",
			"event_id", evt.ID, "event_type", evt.Type)
		return nil
	}
}

// applyCheckoutSessionCompleted persists the (meko user_id,
// stripe customer_id, email) mapping into stripe_customers. The
// session carries client_reference_id (the meko user_id we stamped
// at Checkout-create time) and customer + customer_details.email,
// which is exactly the row shape we need.
//
// No-op when any field is missing -- happens for non-subscription
// checkouts (one-time payments, links) that we don't run today, but
// future code may add. Logging the skip surfaces it without erroring.
func (s *BillingService) applyCheckoutSessionCompleted(
	ctx context.Context, evt *StripeEvent,
) error {
	var sess StripeCheckoutSessionObject
	if err := json.Unmarshal(evt.Data.Object, &sess); err != nil {
		return fmt.Errorf("billing: decode checkout session: %w", err)
	}

	email := sess.CustomerEmail
	if email == "" {
		email = sess.CustomerDetails.Email
	}
	if sess.ClientReferenceID == "" || sess.Customer == "" || email == "" {
		slog.Warn("billing: checkout.session.completed missing fields, skipping customer upsert",
			"session_id", sess.ID,
			"has_user_id", sess.ClientReferenceID != "",
			"has_customer", sess.Customer != "",
			"has_email", email != "")
		return nil
	}

	if err := s.repo.UpsertCustomer(ctx, sess.ClientReferenceID, sess.Customer, email); err != nil {
		return err
	}
	slog.Info("billing: stripe_customers upserted from checkout.session.completed",
		"user_id", sess.ClientReferenceID, "stripe_customer_id", sess.Customer)
	return nil
}

// applySubscriptionEvent decodes the embedded subscription object,
// resolves the Meko user via either Stripe metadata or the
// customer-mapping table, and upserts the local row.
func (s *BillingService) applySubscriptionEvent(
	ctx context.Context, evt *StripeEvent,
) error {
	var sub StripeSubscription
	if err := json.Unmarshal(evt.Data.Object, &sub); err != nil {
		return fmt.Errorf("billing: decode subscription: %w", err)
	}

	userID, err := s.resolveUserIDForSubscription(ctx, &sub)
	if err != nil {
		return err
	}

	priceID := ""
	if len(sub.Items.Data) > 0 {
		priceID = sub.Items.Data[0].Price.ID
	}
	planID := s.planForPrice(priceID)
	// On a status that isn't "paid", downgrade to free regardless of
	// the price line (covers cancellations that arrive as status
	// changes rather than .deleted events).
	if !isPaidStatus(sub.Status) {
		slog.Info("billing: subscription not paid; downgrading",
			"user_id", userID, "status", sub.Status, "sub_id", sub.ID)
		return s.repo.DowngradeToFree(ctx, userID)
	}
	if planID == "" {
		// Price doesn't map to any of our tiers -- probably an
		// off-catalog price set by a manual Stripe operator. Log and
		// keep the user where they were instead of guessing.
		slog.Warn("billing: subscription price unmapped, skipping user_type sync",
			"user_id", userID, "price_id", priceID, "sub_id", sub.ID)
		return nil
	}

	rec := repository.SubscriptionRecord{
		UserID:               userID,
		StripeCustomerID:     sub.Customer,
		StripeSubscriptionID: sub.ID,
		StripePriceID:        priceID,
		Status:               sub.Status,
		PlanID:               planID,
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
	}

	// Stripe moved current_period_* from the top-level Subscription
	// object to each item in 2025-* API versions. Read whichever path
	// has a non-zero value so we work across both shapes.
	startUnix := sub.CurrentPeriodStart
	endUnix := sub.CurrentPeriodEnd
	if len(sub.Items.Data) > 0 {
		if startUnix == 0 {
			startUnix = sub.Items.Data[0].CurrentPeriodStart
		}
		if endUnix == 0 {
			endUnix = sub.Items.Data[0].CurrentPeriodEnd
		}
	}
	if startUnix > 0 {
		t := time.Unix(startUnix, 0).UTC()
		rec.CurrentPeriodStart = &t
	}
	if endUnix > 0 {
		t := time.Unix(endUnix, 0).UTC()
		rec.CurrentPeriodEnd = &t
	}
	return s.repo.UpsertSubscription(ctx, rec)
}

// applySubscriptionDeleted handles customer.subscription.deleted which
// always means "back to free" -- canceled subs never come back as the
// same sub id, the user will start a new subscription via Checkout if
// they re-upgrade.
func (s *BillingService) applySubscriptionDeleted(
	ctx context.Context, evt *StripeEvent,
) error {
	var sub StripeSubscription
	if err := json.Unmarshal(evt.Data.Object, &sub); err != nil {
		return fmt.Errorf("billing: decode deleted sub: %w", err)
	}
	userID, err := s.resolveUserIDForSubscription(ctx, &sub)
	if err != nil {
		return err
	}
	return s.repo.DowngradeToFree(ctx, userID)
}

// resolveUserIDForSubscription tries the metadata-stamped meko_user_id
// first (set at Checkout time) and falls back to the customer-mapping
// table. Either path is acceptable -- the metadata path saves a DB hit
// on the common case where the sub was created through our Checkout.
func (s *BillingService) resolveUserIDForSubscription(
	ctx context.Context, sub *StripeSubscription,
) (string, error) {
	if id, ok := sub.Metadata["meko_user_id"]; ok && id != "" {
		return id, nil
	}
	cust, err := s.repo.GetCustomerByStripeID(ctx, sub.Customer)
	if err != nil {
		return "", err
	}
	if cust == nil {
		return "", fmt.Errorf("billing: no meko user for stripe customer %q", sub.Customer)
	}
	return cust.UserID, nil
}

// priceIDFor maps a Meko plan id to the configured Stripe price id.
// Returns "" for plans without a price (free, enterprise) -- callers
// treat that as ErrUnknownPlan when in the Checkout path.
func (s *BillingService) priceIDFor(planID string) string {
	switch planID {
	case "starter":
		return s.cfg.PriceIDStarter
	case "growth":
		return s.cfg.PriceIDGrowth
	case "scale":
		return s.cfg.PriceIDScale
	default:
		return ""
	}
}

// planForPrice is the reverse mapping used by the webhook handler when
// it has a price id and needs to set users.user_type.
func (s *BillingService) planForPrice(priceID string) string {
	switch priceID {
	case s.cfg.PriceIDStarter:
		return "starter"
	case s.cfg.PriceIDGrowth:
		return "growth"
	case s.cfg.PriceIDScale:
		return "scale"
	default:
		return ""
	}
}

// isPaidStatus reports whether a Stripe subscription status keeps the
// user on their paid tier. trialing counts as paid for UI purposes;
// past_due / unpaid / canceled / paused / incomplete do not.
func isPaidStatus(s string) bool {
	switch s {
	case "active", "trialing":
		return true
	default:
		return false
	}
}
