package handlers

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// redirectBaseFromRequest extracts a safe UI base URL (scheme + host)
// from the request so Stripe redirects land back on whichever origin
// the user is actually using -- localhost in dev, cloud.mekodata.ai
// in prod, app.mekodev.com on staging. Returns "" when neither header
// is present OR the supplied host is not on the allowlist; in either
// case BillingService falls back to cfg.AppURL.
//
// SECURITY: Origin / Referer are caller-controlled and a forged value
// would otherwise turn /billing/checkout-session into an open redirect
// (attacker CSRFs a victim into starting a real Stripe checkout, then
// post-completion Stripe redirects the victim to an attacker-owned
// page that can phish further credentials). The allowlist below is
// what closes that hole. The legitimate `appURL` host is always
// allowed; localhost variants are allowed for local development since
// an attacker on the public internet cannot host their phishing page
// on `localhost` from the victim's perspective.
func redirectBaseFromRequest(c echo.Context, appURL string) string {
	appHost := hostnameOf(appURL)
	candidates := []string{
		c.Request().Header.Get("Origin"),
		c.Request().Header.Get("Referer"),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		if isAllowedRedirectHost(u.Hostname(), appHost) {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

// isAllowedRedirectHost gates the redirect host against the configured
// AppURL plus localhost loopbacks (for dev). Exact-match on hostname
// only -- subdomains of mekodata.ai are intentionally rejected.
func isAllowedRedirectHost(host, appHost string) bool {
	if host == "" {
		return false
	}
	if appHost != "" && host == appHost {
		return true
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// hostnameOf parses a URL string and returns its hostname (no port),
// or "" when the input is empty / unparseable.
func hostnameOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// webhookMaxBodyBytes caps the raw body we'll ReadAll from a Stripe
// webhook POST. Sized generously above the observed payload size
// (~40KB for a fat invoice event)
const webhookMaxBodyBytes int64 = 256 * 1024

// BillingHandler exposes the Stripe Checkout / Portal / webhook
// endpoints. All three are thin wrappers around BillingService.
type BillingHandler struct {
	svc *service.BillingService
}

// NewBillingHandler creates a new BillingHandler.
func NewBillingHandler(svc *service.BillingService) *BillingHandler {
	return &BillingHandler{svc: svc}
}

// checkoutSessionRequest is the JSON body for POST /billing/checkout-session.
type checkoutSessionRequest struct {
	// PlanID is one of "starter" | "growth" | "scale". Free and
	// enterprise are not Checkout-eligible -- the handler 400s.
	PlanID string `json:"plan_id"`
}

// CreateCheckoutSession handles POST /api/v1/billing/checkout-session.
//
//	@Summary	Create a Stripe Checkout session for a plan upgrade
//	@Tags		billing
//	@Accept		json
//	@Produce	json
//	@Param		body	body		checkoutSessionRequest	true	"Checkout request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/billing/checkout-session [post]
func (h *BillingHandler) CreateCheckoutSession(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	var req checkoutSessionRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if req.PlanID == "" {
		return apiError(c, http.StatusBadRequest,
			"MISSING_PLAN_ID", "plan_id is required")
	}

	out, err := h.svc.CreateCheckoutSession(c.Request().Context(), service.CheckoutInput{
		UserID:  account.ID,
		Email:   account.Email,
		PlanID:  req.PlanID,
		BaseURL: redirectBaseFromRequest(c, h.svc.AppURL()),
	})
	if err != nil {
		if errors.Is(err, service.ErrUnknownPlan) {
			return apiError(c, http.StatusBadRequest,
				"UNKNOWN_PLAN", "no upgrade flow exists for that plan")
		}
		if errors.Is(err, service.ErrAlreadySubscribed) {
			// 409: caller has an active subscription. The UI should
			// route them to the Customer Portal for plan changes
			// instead of opening Checkout again.
			return apiError(c, http.StatusConflict,
				"ALREADY_SUBSCRIBED",
				"you already have an active subscription; use Manage payment to switch plans")
		}
		slog.Error("billing: create checkout session failed",
			"user_id", account.ID, "plan_id", req.PlanID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"CHECKOUT_FAILED", "could not start checkout")
	}
	return ok(c, http.StatusOK, map[string]string{
		"url": out.URL,
	})
}

// CreatePortalSession handles POST /api/v1/billing/portal-session.
//
//	@Summary	Create a Stripe Customer Portal session
//	@Tags		billing
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	409	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/billing/portal-session [post]
func (h *BillingHandler) CreatePortalSession(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	portalURL, err := h.svc.CreatePortalSession(c.Request().Context(),
		account.ID, redirectBaseFromRequest(c, h.svc.AppURL()))
	if err != nil {
		if errors.Is(err, service.ErrCustomerNotFound) {
			// 409 (not 404) because the user exists -- the missing
			// resource is the Stripe customer mapping, which the UI
			// surfaces as "upgrade first".
			return apiError(c, http.StatusConflict,
				"NO_STRIPE_CUSTOMER", "no billing account yet; upgrade to a paid plan first")
		}
		slog.Error("billing: create portal session failed",
			"user_id", account.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"PORTAL_FAILED", "could not open billing portal")
	}
	return ok(c, http.StatusOK, map[string]string{
		"url": portalURL,
	})
}

// Summary handles GET /api/v1/billing/summary.
//
// Lightweight read used by the Plans & billing page to populate the
// "Renews" date and "Card on file" pill on the current-plan card.
// Reads the subscription row from our DB and fetches the default
// payment method from Stripe in a single round-trip per call.
//
//	@Summary	Billing summary for the current plan card
//	@Tags		billing
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/billing/summary [get]
func (h *BillingHandler) Summary(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	sum, err := h.svc.GetBillingSummary(c.Request().Context(), account.ID)
	if err != nil {
		slog.Error("billing: get summary failed",
			"user_id", account.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"SUMMARY_FAILED", "could not load billing summary")
	}

	// Flatten to a JSON-friendly shape. Empty fields render as the
	// natural absent state on the FE without a special "null" case.
	resp := map[string]any{
		"plan_id":              sum.PlanID,
		"status":               sum.Status,
		"cancel_at_period_end": sum.CancelAtPeriodEnd,
		"card_brand":           sum.CardBrand,
		"card_last4":           sum.CardLast4,
	}
	if sum.CurrentPeriodEnd != nil {
		resp["current_period_end"] = sum.CurrentPeriodEnd.UTC().Format("2006-01-02T15:04:05Z")
	}
	return ok(c, http.StatusOK, resp)
}

// Webhook handles POST /api/v1/billing/webhook.
//
// Unlike the other two billing routes this is unauthenticated -- the
// signature header is the only credential. The route MUST be
// registered without the requireAuth middleware or Stripe's calls
// will 401. Stripe times out at 30s and retries on non-2xx, so we
// keep the handler synchronous but bounded.
//
//	@Summary	Stripe webhook receiver (no JWT, Stripe-Signature verified)
//	@Tags		billing
//	@Accept		json
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Router		/billing/webhook [post]
func (h *BillingHandler) Webhook(c echo.Context) error {
	// DoS guard: cap the read to webhookMaxBodyBytes (256KB). Webhook
	// payloads in practice are well under 64KB (a fat invoice event
	// with line items is ~40KB), but Stripe doesn't document a hard
	// ceiling, so 256KB leaves headroom
	r := c.Request()
	r.Body = http.MaxBytesReader(c.Response().Writer, r.Body, webhookMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return apiError(c, http.StatusBadRequest,
			"BAD_BODY", "could not read webhook body")
	}

	sig := c.Request().Header.Get("Stripe-Signature")
	if sig == "" {
		return apiError(c, http.StatusBadRequest,
			"MISSING_SIGNATURE", "Stripe-Signature header is required")
	}

	if err := h.svc.HandleWebhook(c.Request().Context(), sig, body); err != nil {
		// Log + 400. Stripe will retry; if the signature itself is
		// wrong (rotated secret, replay, spoofed call) it will keep
		// retrying and we'll see the noise -- preferable to silently
		// accepting an unverified payload.
		slog.Warn("billing: webhook handling failed", "error", err)
		return apiError(c, http.StatusBadRequest,
			"WEBHOOK_REJECTED", "webhook was not accepted")
	}
	return ok(c, http.StatusOK, map[string]string{"received": "ok"})
}
