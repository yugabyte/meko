// Package handlers — AWS Marketplace fulfillment + SNS endpoints.
//
// Three routes total:
//
//	POST /marketplace/register  — PUBLIC. Fulfillment URL AWS form-POSTs
//	                              to after subscription. Stashes the
//	                              opaque token in a cookie and bounces
//	                              to /signup (new buyer) or
//	                              /marketplace/complete (existing user).
//	GET  /marketplace/complete  — AUTHED. Reads the stashed token,
//	                              resolves the entitlement, flips
//	                              user_type, redirects to welcome URL.
//	POST /marketplace/sns       — PUBLIC. SNS entitlement-change
//	                              notifications; verified by signature.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// MarketplaceHandler groups the two Marketplace endpoints. The
// service field is nil when the operator hasn't configured a
// ProductCode yet; both handlers 503 in that state.
type MarketplaceHandler struct {
	svc         *service.MarketplaceService
	repo        *repository.MarketplaceRepo
	billingRepo *repository.BillingRepo

	// welcomeURL is where Complete redirects the user after a
	// successful entitlement resolution. Set via
	// AWS_MARKETPLACE_WELCOME_URL. Also used to derive the FE origin
	// for the intermediate /marketplace/complete bounce. Falls back
	// to "/" when unset.
	welcomeURL string
}

func NewMarketplaceHandler(
	svc *service.MarketplaceService,
	repo *repository.MarketplaceRepo,
	billingRepo *repository.BillingRepo,
	welcomeURL string,
) *MarketplaceHandler {
	if welcomeURL == "" {
		welcomeURL = "/"
	}
	return &MarketplaceHandler{
		svc: svc, repo: repo, billingRepo: billingRepo, welcomeURL: welcomeURL,
	}
}

// Cookie carrying the AWS-issued single-use registration token between
// the public Register POST and the authed Complete GET. Short TTL so a
// forgotten flow doesn't sit in the browser indefinitely; AWS tokens
// are single-use so once Complete consumes it a stale copy is inert.
const (
	marketplaceTokenCookieName = "mko_mkp_token"
	marketplaceTokenCookieTTL  = 15 * time.Minute
)

// FE landing path the fulfillment redirect bounces through. Prefixed
// with the FE origin at redirect time so browsers don't resolve it
// against the api_server host (relevant in local dev / ngrok setups
// where FE and BE live on different ports and cookies don't cross).
//
// The token is passed as a URL query param `?t=<token>` rather than
// a cookie because cookies don't survive the ngrok → localhost origin
// hop; the token is single-use so URL exposure is bounded.
const completePathForMarketplace = "/marketplace/complete"

// feBase returns the scheme+host portion of the configured welcome URL
// so redirects can be made absolute. Falls back to "" when welcomeURL
// isn't a parseable absolute URL; callers should then use the raw path
// and hope FE and BE share an origin.
func (h *MarketplaceHandler) feBase() string {
	u, err := url.Parse(h.welcomeURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Register handles POST /api/v1/marketplace/register (PUBLIC — AWS
// Marketplace form-POSTs here as a top-level browser navigation, and
// the buyer may not have a Meko session yet). Body:
//
//	x-amzn-marketplace-token=<opaque-single-use-token>
//
// The handler stashes the token in a short-lived cookie and bounces:
//   - If the browser already carries a Meko `access_token` cookie
//     (existing user) -> redirect to /marketplace/complete which
//     validates the session and finishes the registration.
//   - Otherwise (brand-new buyer arriving from AWS) -> redirect to
//     /signup with return_to=/marketplace/complete so the post-signup
//     navigation replays the entitlement lookup.
//
// The actual AWS calls (ResolveCustomer / GetEntitlement) run only
// in Complete, so we never call AWS with a token belonging to a user
// we can't yet identify.
//
//	@Summary	AWS Marketplace fulfillment redirect
//	@Tags		marketplace
//	@Accept		application/x-www-form-urlencoded
//	@Produce	json
//	@Success	303	{string}	string	"redirect to signup or /marketplace/complete"
//	@Failure	400	{object}	APIResponse
//	@Failure	503	{object}	APIResponse
//	@Router		/marketplace/register [post]
func (h *MarketplaceHandler) Register(c echo.Context) error {
	if h.svc == nil || !h.svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"MARKETPLACE_DISABLED", "AWS Marketplace is not configured")
	}

	token := strings.TrimSpace(c.FormValue("x-amzn-marketplace-token"))
	if token == "" {
		// Some AWS docs also show the token arriving as a query param
		// during manual testing; accept both to keep dev-loops smooth.
		token = strings.TrimSpace(c.QueryParam("x-amzn-marketplace-token"))
	}
	if token == "" {
		return apiError(c, http.StatusBadRequest,
			"MISSING_TOKEN", "x-amzn-marketplace-token is required")
	}

	// Bounce to the FE completion route with the token in the URL.
	// The FE (which is same-origin with the auth cookie) then calls
	// /api/v1/marketplace/complete to finalize; if the user isn't
	// signed in yet, the FE route redirects to /signup with the
	// return_to carrying the token forward.
	target := completePathForMarketplace + "?t=" + url.QueryEscape(token)
	if base := h.feBase(); base != "" {
		target = base + target
	}
	slog.Info("marketplace register: bouncing", "target", target)
	return c.Redirect(http.StatusSeeOther, target)
}

// Complete handles GET /api/v1/marketplace/complete (AUTHED). Reads
// the token stashed by Register, resolves the entitlement, flips
// user_type, and redirects to the welcome URL. Callers reach it
// either directly (existing user) or via /signup?return_to=... (new
// buyer). Always clears the cookie so a back-navigate can't replay.
//
//	@Summary	Finalize an AWS Marketplace registration
//	@Tags		marketplace
//	@Produce	json
//	@Success	303	{string}	string	"redirect to welcome URL"
//	@Failure	400	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	409	{object}	APIResponse
//	@Failure	502	{object}	APIResponse
//	@Failure	503	{object}	APIResponse
//	@Router		/marketplace/complete [get]
func (h *MarketplaceHandler) Complete(c echo.Context) error {
	if h.svc == nil || !h.svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"MARKETPLACE_DISABLED", "AWS Marketplace is not configured")
	}

	acct, err := auth.GetAccount(c)
	if err != nil || acct == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	// Token comes from ?t=<token> on the FE-side URL. Falls back to the
	// legacy cookie name so any in-flight cookie-based sessions still
	// complete instead of 400-ing.
	token := strings.TrimSpace(c.QueryParam("t"))
	if token == "" {
		if cookie, cErr := c.Cookie(marketplaceTokenCookieName); cErr == nil && cookie != nil {
			token = strings.TrimSpace(cookie.Value)
		}
	}
	if token == "" {
		return apiError(c, http.StatusBadRequest,
			"MISSING_TOKEN", "no AWS Marketplace token to complete; start from the Marketplace listing")
	}
	clearMarketplaceTokenCookie(c)

	planID, err := h.finalizeEntitlement(c.Request().Context(), acct.ID, token)
	if err != nil {
		return err // helper returns an already-formatted apiError
	}

	target := welcomeURLWithParams(h.welcomeURL, planID)
	return c.Redirect(http.StatusSeeOther, target)
}

// finalizeEntitlement runs the resolve+entitlement+upsert+setUserType
// sequence shared by Complete (post-signup) and potentially SNS. On
// AWS-side failures returns an echo.HTTPError-shaped result so
// callers just return it up.
func (h *MarketplaceHandler) finalizeEntitlement(
	ctx context.Context, userID, token string,
) (string, error) {
	resolved, err := h.svc.ResolveCustomer(ctx, token)
	if err != nil {
		slog.Error("marketplace complete: resolve customer failed",
			"user_id", userID, "error", err)
		return "", newAPIError(http.StatusBadGateway,
			"AWS_ERROR", "failed to resolve Marketplace customer")
	}

	snap, err := h.svc.GetEntitlement(ctx, resolved.CustomerIdentifier)
	if err != nil {
		slog.Error("marketplace complete: get entitlement failed",
			"user_id", userID, "aws_customer_id", resolved.CustomerIdentifier, "error", err)
		return "", newAPIError(http.StatusBadGateway,
			"AWS_ERROR", "failed to fetch entitlements from AWS")
	}
	if snap == nil {
		return "", newAPIError(http.StatusConflict,
			"NO_ENTITLEMENT", "AWS reports no active entitlement for this customer")
	}

	planID := service.PlanIDFromDimension(snap.Dimension)
	if planID == "" {
		slog.Error("marketplace complete: unknown dimension",
			"user_id", userID, "dimension", snap.Dimension)
		return "", newAPIError(http.StatusInternalServerError,
			"UNKNOWN_DIMENSION", "AWS returned an unknown plan dimension")
	}

	ent := repository.MarketplaceEntitlement{
		UserID:        userID,
		AWSCustomerID: resolved.CustomerIdentifier,
		AWSAccountID:  resolved.CustomerAWSAccount,
		ProductCode:   resolved.ProductCode,
		PlanID:        planID,
		Status:        snap.Status,
	}
	if !snap.ExpiresAt.IsZero() {
		t := snap.ExpiresAt
		ent.ExpiresAt = &t
	}
	if err := h.repo.Upsert(ctx, ent); err != nil {
		slog.Error("marketplace complete: upsert failed",
			"user_id", userID, "error", err)
		return "", newAPIError(http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to persist entitlement")
	}
	if err := h.setUserType(ctx, userID, planID); err != nil {
		slog.Error("marketplace complete: set user_type failed",
			"user_id", userID, "plan_id", planID, "error", err)
		// Non-fatal — entitlement row is authoritative; reconcile job catches drift.
	}

	slog.Info("marketplace: entitlement registered",
		"user_id", userID, "plan_id", planID,
		"aws_customer_id", resolved.CustomerIdentifier)
	return planID, nil
}

// welcomeURLWithParams appends the fixed marketing tracking params so
// the FE landing page can render a "welcome to Standard" pill.
func welcomeURLWithParams(base, planID string) string {
	if u, err := url.Parse(base); err == nil {
		q := u.Query()
		q.Set("source", "aws-marketplace")
		q.Set("plan", planID)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return base
}

// clearMarketplaceTokenCookie zeroes the legacy `mko_mkp_token` cookie
// on the successful /marketplace/complete hop so a stale value from an
// older code path can't linger and re-fire on a later visit. The set
// side (setMarketplaceTokenCookie) is gone -- Register now passes the
// token via ?t=<token> and never writes the cookie -- but we still read
// the cookie as a fallback for in-flight sessions and must clear it
// once consumed.
func clearMarketplaceTokenCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     marketplaceTokenCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(c),
		MaxAge:   -1,
	})
}

// newAPIError wraps an echo.HTTPError so the shared finalize helper
// can return handler-shaped errors up to the caller.
func newAPIError(status int, code, message string) *echo.HTTPError {
	return echo.NewHTTPError(status, APIResponse{
		Error: &APIError{Code: code, Message: message},
	})
}

// SNS handles POST /api/v1/marketplace/sns.
//
// AWS SNS delivers two message shapes here:
//
//   - SubscriptionConfirmation (only at wire-up time): body includes a
//     SubscribeURL we must GET to confirm the subscription. Unauthenticated —
//     verified via SNS X.509 signature.
//   - Notification: an entitlement-change payload. The `Message`
//     field is JSON containing `customer-identifier`, `action`, and
//     `product-code`. On any change we re-run GetEntitlement and
//     upsert.
//
// Every payload's authenticity is checked via AWS-issued X.509
// signature before any side effect runs. See sns_verify.go for the
// canonical-string rebuild + RSA verify.
//
//	@Summary	AWS Marketplace SNS entitlement notifications
//	@Tags		marketplace
//	@Accept		json
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	503	{object}	APIResponse
//	@Router		/marketplace/sns [post]
func (h *MarketplaceHandler) SNS(c echo.Context) error {
	if h.svc == nil || !h.svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"MARKETPLACE_DISABLED", "AWS Marketplace is not configured")
	}

	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_BODY", "failed to read request body")
	}

	var msg snsMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed SNS envelope")
	}

	// Verify BEFORE any side effect. A fake payload posted by an
	// attacker would otherwise be able to fire the SubscribeURL fetch
	// or trigger our entitlement resolution against a spoofed
	// customer identifier.
	if err := verifySNSSignature(&msg); err != nil {
		slog.Warn("marketplace sns: signature verification failed",
			"message_id", msg.MessageID, "topic", msg.TopicARN, "error", err)
		return apiError(c, http.StatusUnauthorized,
			"INVALID_SIGNATURE", "SNS message signature did not verify")
	}

	switch msg.Type {
	case "SubscriptionConfirmation":
		// One-shot: hit SubscribeURL to confirm this SNS subscription.
		// AWS won't deliver notifications until we do. Signature was
		// already verified above, so we can trust SubscribeURL.
		if msg.SubscribeURL == "" {
			return apiError(c, http.StatusBadRequest,
				"MISSING_SUBSCRIBE_URL", "SubscribeURL missing on confirmation")
		}
		if _, err := http.Get(msg.SubscribeURL); err != nil { //nolint:gosec // AWS-controlled URL, one-shot
			slog.Error("marketplace sns: subscription confirm failed",
				"topic", msg.TopicARN, "error", err)
			return apiError(c, http.StatusBadGateway,
				"AWS_ERROR", "failed to confirm SNS subscription")
		}
		slog.Info("marketplace sns: subscription confirmed", "topic", msg.TopicARN)
		return c.NoContent(http.StatusOK)

	case "Notification":
		return h.handleEntitlementChange(c.Request().Context(), msg.MessageID, msg.Message)

	case "UnsubscribeConfirmation":
		// Someone unsubscribed our endpoint from the topic. Nothing
		// to do — log so operators notice.
		slog.Warn("marketplace sns: unsubscribed from topic", "topic", msg.TopicARN)
		return c.NoContent(http.StatusOK)

	default:
		slog.Warn("marketplace sns: unknown type", "type", msg.Type)
		return c.NoContent(http.StatusOK)
	}
}

// handleEntitlementChange parses the SNS Message JSON, looks up the
// mapped Meko user by aws_customer_id, and re-resolves the current
// entitlement. Upgrades / downgrades / cancellations all land here.
func (h *MarketplaceHandler) handleEntitlementChange(
	ctx context.Context, messageID, message string,
) error {
	var payload struct {
		Action             string `json:"action"`
		CustomerIdentifier string `json:"customer-identifier"`
		ProductCode        string `json:"product-code"`
	}
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		return fmt.Errorf("sns: parse message: %w", err)
	}
	if payload.CustomerIdentifier == "" {
		slog.Warn("marketplace sns: missing customer identifier", "message_id", messageID)
		return nil
	}

	existing, err := h.repo.GetByAWSCustomerID(ctx, payload.CustomerIdentifier)
	if err != nil {
		return fmt.Errorf("sns: lookup by aws customer id: %w", err)
	}
	if existing == nil {
		// SNS event for a customer we never registered — possible if
		// the buyer skipped our fulfillment URL somehow, or if this
		// SNS delivery races the register call. Log and no-op; the
		// register call will pick up the current state next time.
		slog.Warn("marketplace sns: unknown customer identifier",
			"aws_customer_id", payload.CustomerIdentifier, "action", payload.Action)
		return nil
	}

	snap, err := h.svc.GetEntitlement(ctx, payload.CustomerIdentifier)
	if err != nil {
		return fmt.Errorf("sns: get entitlement: %w", err)
	}

	newPlanID := existing.PlanID
	newStatus := service.MarketplaceStatusEntitled
	var expiresAt *time.Time //nolint:staticcheck // set below
	if snap == nil {
		// AWS reports no active entitlement — treat as cancellation.
		newStatus = service.MarketplaceStatusExpired
	} else {
		newPlanID = service.PlanIDFromDimension(snap.Dimension)
		newStatus = snap.Status
		if !snap.ExpiresAt.IsZero() {
			t := snap.ExpiresAt
			expiresAt = &t
		}
	}
	if newPlanID == "" {
		newPlanID = existing.PlanID
	}

	updated := repository.MarketplaceEntitlement{
		UserID:        existing.UserID,
		AWSCustomerID: existing.AWSCustomerID,
		AWSAccountID:  existing.AWSAccountID,
		ProductCode:   existing.ProductCode,
		PlanID:        newPlanID,
		Status:        newStatus,
		ExpiresAt:     expiresAt,
	}
	if err := h.repo.Upsert(ctx, updated); err != nil {
		return fmt.Errorf("sns: upsert: %w", err)
	}
	// If the entitlement is no longer ENTITLED, drop the user back
	// to free; otherwise reflect the new tier on users.user_type.
	target := newPlanID
	if newStatus != service.MarketplaceStatusEntitled {
		target = "free"
	}
	if err := h.setUserType(ctx, existing.UserID, target); err != nil {
		slog.Error("marketplace sns: set user_type failed",
			"user_id", existing.UserID, "target", target, "error", err)
	}
	slog.Info("marketplace sns: entitlement updated",
		"user_id", existing.UserID, "plan_id", newPlanID, "status", newStatus,
		"action", payload.Action, "message_id", messageID)
	return nil
}

// setUserType flips meko_system.users.user_type via a raw UPDATE.
// Kept here (rather than in a BillingRepo method) so the Marketplace
// path doesn't have to import Stripe-domain types. When `plan` is
// "free" we drop back to the free tier the same way Stripe cancellation
// does.
func (h *MarketplaceHandler) setUserType(ctx context.Context, userID, plan string) error {
	if h.billingRepo == nil {
		return nil
	}
	if plan == "free" {
		return h.billingRepo.SetUserTypeFree(ctx, userID)
	}
	return h.billingRepo.SetUserTypePlan(ctx, userID, plan)
}
