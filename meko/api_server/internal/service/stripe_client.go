package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// stripeAPIBase is the public Stripe REST endpoint. Overridable via the
// constructor so tests can point at a local fake without env juggling.
const stripeAPIBase = "https://api.stripe.com/v1"

// stripeSignatureToleranceSeconds is the max skew between the timestamp
// in the Stripe-Signature header and our server clock. Five minutes
// matches Stripe's documented replay-protection window.
const stripeSignatureToleranceSeconds int64 = 300

// StripeClient is a thin REST wrapper over the few Stripe endpoints
// the billing flow needs. We deliberately stay below the stripe-go
// SDK because three endpoints + one HMAC verifier is far less code
// (and dependency surface) than pulling in the full SDK tree.
type StripeClient struct {
	secretKey     string
	webhookSecret string
	httpClient    *http.Client
	apiBase       string
}

// NewStripeClient builds a client from a secret key and webhook signing
// secret. Both come from the Stripe dashboard; the webhook secret is
// per-endpoint (one per env), the API secret is per-account.
func NewStripeClient(secretKey, webhookSecret string) *StripeClient {
	return &StripeClient{
		secretKey:     secretKey,
		webhookSecret: webhookSecret,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		apiBase:       stripeAPIBase,
	}
}

// Enabled reports whether the client has the credentials it needs to
// make API calls. Both the secret key and the webhook signing secret
// are required: without the latter we'd accept unverified webhook
// payloads, which would let any caller spoof a plan upgrade.
func (c *StripeClient) Enabled() bool {
	return c != nil && c.secretKey != "" && c.webhookSecret != ""
}

// CheckoutSessionInput collects the fields needed for a subscription
// Checkout session. We always run in subscription mode -- one-shot
// payments aren't part of the billing model.
type CheckoutSessionInput struct {
	// CustomerID is required when present: it ensures the new
	// subscription attaches to an existing Stripe customer instead of
	// minting a duplicate when the user has paid before.
	CustomerID string
	// CustomerEmail is used only when CustomerID is empty -- Stripe
	// creates a new customer with this email at session-confirm time.
	CustomerEmail string
	PriceID       string
	SuccessURL    string
	CancelURL     string
	// ClientReferenceID is echoed back on the completed event so the
	// webhook handler can resolve the Meko user_id without an extra
	// customer lookup. We set it to the Meko user_id at the caller.
	ClientReferenceID string
}

// CheckoutSession is the slim shape we surface to callers; the real
// Stripe response carries 100+ fields we don't need.
type CheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateCheckoutSession POSTs to /v1/checkout/sessions in subscription
// mode and returns the hosted-page URL the UI should redirect to.
func (c *StripeClient) CreateCheckoutSession(
	ctx context.Context, in CheckoutSessionInput,
) (*CheckoutSession, error) {
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("success_url", in.SuccessURL)
	form.Set("cancel_url", in.CancelURL)
	form.Set("line_items[0][price]", in.PriceID)
	form.Set("line_items[0][quantity]", "1")
	if in.CustomerID != "" {
		form.Set("customer", in.CustomerID)
		// Allow the user to update their email in the portal without
		// the session refusing to load. Only valid when we attach an
		// existing customer; new-customer flows ignore this.
		form.Set("customer_update[address]", "auto")
	} else if in.CustomerEmail != "" {
		form.Set("customer_email", in.CustomerEmail)
	}
	if in.ClientReferenceID != "" {
		form.Set("client_reference_id", in.ClientReferenceID)
	}
	// Echoes the Meko user_id back on every webhook for this sub --
	// belt-and-suspenders with client_reference_id since the latter is
	// only on the checkout.session.completed event.
	if in.ClientReferenceID != "" {
		form.Set("subscription_data[metadata][meko_user_id]", in.ClientReferenceID)
	}

	var out CheckoutSession
	if err := c.postForm(ctx, "/checkout/sessions", form, &out); err != nil {
		return nil, fmt.Errorf("stripe: create checkout session: %w", err)
	}
	return &out, nil
}

// PortalSession is the slim shape we surface for portal sessions.
type PortalSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// StripeCardOnFile is the slim shape returned by GetDefaultCard --
// just enough to render "Visa ending 4242" in the UI.
type StripeCardOnFile struct {
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
}

// stripePaymentMethodList mirrors the GET /v1/customers/{id}/payment_methods
// response shape -- a List object with a `data` array. Only the card
// brand + last4 are decoded; metadata, billing details, etc. are
// ignored. The list itself contains every card attached to the
// customer; we read the first entry as "the" card on file because
// subscription-Checkout flows attach exactly one.
type stripePaymentMethodList struct {
	Data []struct {
		ID   string            `json:"id"`
		Card *StripeCardOnFile `json:"card"`
	} `json:"data"`
}

// GetDefaultCard returns the card the customer most recently attached
// (brand + last4) or (nil, nil) when no card is on file. Lists the
// customer's payment methods directly rather than reading the
// `invoice_settings.default_payment_method` field -- Stripe Checkout
// in subscription mode attaches the card to the *subscription*, not
// to the customer's invoice_settings, so the latter is typically nil.
//
// Listing the methods works regardless of which "default" Stripe set
// where: every Checkout flow leaves at least one card attached, and
// the list path returns them all sorted by created time.
//
// Used by GetBillingSummary to populate the "Card on file" pill on
// the Plans & billing page without forcing a Customer-Portal round
// trip just to see what card is attached.
func (c *StripeClient) GetDefaultCard(
	ctx context.Context, customerID string,
) (*StripeCardOnFile, error) {
	if c.secretKey == "" {
		return nil, errors.New("stripe secret key not configured")
	}

	q := url.Values{}
	q.Set("type", "card")
	q.Set("limit", "1")
	endpoint := c.apiBase + "/customers/" + url.PathEscape(customerID) +
		"/payment_methods?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Stripe-Version", "2024-06-20")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe: list payment methods: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("stripe: read payment methods response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe %d: %s", resp.StatusCode, string(body))
	}

	var out stripePaymentMethodList
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("stripe: decode payment methods: %w", err)
	}
	if len(out.Data) == 0 || out.Data[0].Card == nil {
		return nil, nil
	}
	return out.Data[0].Card, nil
}

// CreatePortalSession POSTs to /v1/billing_portal/sessions and returns
// a one-time-use URL the UI redirects the user to. The portal hosts
// payment-method updates, invoice downloads, and plan cancellation --
// no extra UI work on our side.
func (c *StripeClient) CreatePortalSession(
	ctx context.Context, customerID, returnURL string,
) (*PortalSession, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)

	var out PortalSession
	if err := c.postForm(ctx, "/billing_portal/sessions", form, &out); err != nil {
		return nil, fmt.Errorf("stripe: create portal session: %w", err)
	}
	return &out, nil
}

// StripeEvent is the slim shape of a webhook event after signature
// verification + JSON parse. Only the fields the BillingService cares
// about are surfaced.
type StripeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data StripeEventData `json:"data"`
}

// StripeEventData wraps the actual object Stripe sent. Object is the
// raw JSON so the caller can decode it into the right concrete type
// based on Type (subscription / invoice / checkout session).
type StripeEventData struct {
	Object json.RawMessage `json:"object"`
}

// StripeSubscription is the slim shape of a subscription object on
// customer.subscription.* events. Only the fields used by the
// BillingService state-sync path are surfaced.
type StripeSubscription struct {
	ID                 string                  `json:"id"`
	Customer           string                  `json:"customer"`
	Status             string                  `json:"status"`
	CurrentPeriodStart int64                   `json:"current_period_start"`
	CurrentPeriodEnd   int64                   `json:"current_period_end"`
	CancelAtPeriodEnd  bool                    `json:"cancel_at_period_end"`
	Items              StripeSubscriptionItems `json:"items"`
	Metadata           map[string]string       `json:"metadata"`
}

// StripeSubscriptionItems mirrors the nested `items.data[].price` path
// since that's where the active price id lives on the sub object.
type StripeSubscriptionItems struct {
	Data []StripeSubscriptionItem `json:"data"`
}

// StripeSubscriptionItem carries the price object for one subscribed
// line. Multi-line subs aren't part of the Meko billing model -- the
// first item wins.
//
// CurrentPeriodStart / CurrentPeriodEnd were moved from the top-level
// Subscription object to each item in the 2025-* Stripe API versions
// (multi-item subs can now have per-item billing periods). We read
// from here when the top-level fields come back as zero so the
// handler works across both old and new payload shapes.
type StripeSubscriptionItem struct {
	Price              StripePrice `json:"price"`
	CurrentPeriodStart int64       `json:"current_period_start"`
	CurrentPeriodEnd   int64       `json:"current_period_end"`
}

// StripePrice is the subset of the Stripe Price object the
// BillingService uses to map onto a Meko plan tier.
type StripePrice struct {
	ID string `json:"id"`
}

// StripeCheckoutSessionObject is the slim shape of a
// checkout.session.completed event payload. We use it to populate
// the stripe_customers mapping right after a successful Checkout --
// the subscription events come a few hundred ms later but the
// session event is the cleanest place to capture (user_id, customer
// id, email) in a single payload.
type StripeCheckoutSessionObject struct {
	ID                string                       `json:"id"`
	Customer          string                       `json:"customer"`
	ClientReferenceID string                       `json:"client_reference_id"`
	CustomerEmail     string                       `json:"customer_email"`
	CustomerDetails   StripeCheckoutCustomerDetail `json:"customer_details"`
}

// StripeCheckoutCustomerDetail mirrors the nested
// customer_details.email path on the checkout session -- Stripe
// surfaces the buyer's email there for guest checkouts when no
// pre-existing Customer is attached.
type StripeCheckoutCustomerDetail struct {
	Email string `json:"email"`
}

// VerifyWebhookSignature checks the Stripe-Signature header against the
// raw request body using the constructor-supplied webhook secret. The
// header has the form "t=<unix_ts>,v1=<hex_sig>,v0=<legacy_hex>"; we
// recompute HMAC-SHA256 over "<ts>.<body>" with the secret and compare
// in constant time against every v1 entry (Stripe rotates secrets via
// dual-signed deliveries during a roll).
//
// Returns the parsed event when the signature checks out so callers
// don't have to JSON-decode the body a second time.
func (c *StripeClient) VerifyWebhookSignature(
	signatureHeader string, body []byte, now time.Time,
) (*StripeEvent, error) {
	if c.webhookSecret == "" {
		return nil, errors.New("stripe webhook secret not configured")
	}

	var headerTimestamp int64
	var signatures []string
	for _, part := range strings.Split(signatureHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("stripe: bad timestamp in signature header: %w", err)
			}
			headerTimestamp = ts
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}
	if headerTimestamp == 0 || len(signatures) == 0 {
		return nil, errors.New("stripe: signature header missing t / v1")
	}

	// Replay protection: reject anything outside the documented
	// tolerance window. Clock skew between Stripe and our pod has to
	// stay under five minutes for webhooks to be accepted.
	skew := now.Unix() - headerTimestamp
	if skew < 0 {
		skew = -skew
	}
	if skew > stripeSignatureToleranceSeconds {
		return nil, fmt.Errorf("stripe: signature timestamp outside tolerance (skew=%ds)", skew)
	}

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	fmt.Fprintf(mac, "%d.", headerTimestamp)
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, sigHex := range signatures {
		got, err := hex.DecodeString(sigHex)
		if err != nil {
			continue
		}
		if hmac.Equal(expected, got) {
			var evt StripeEvent
			if jerr := json.Unmarshal(body, &evt); jerr != nil {
				return nil, fmt.Errorf("stripe: webhook body parse: %w", jerr)
			}
			return &evt, nil
		}
	}
	return nil, errors.New("stripe: signature mismatch")
}

// postForm is the shared transport helper for the two POST endpoints.
// On non-2xx it surfaces Stripe's error body in the returned error so
// the cause is visible in api_server logs without an extra round-trip
// to the Stripe dashboard.
func (c *StripeClient) postForm(
	ctx context.Context, path string, form url.Values, out any,
) error {
	if c.secretKey == "" {
		return errors.New("stripe secret key not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Pin the API version so future Stripe changes don't silently
	// rename fields the BillingService relies on. Bump deliberately
	// when we want to adopt new behaviour.
	req.Header.Set("Stripe-Version", "2024-06-20")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("stripe %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
