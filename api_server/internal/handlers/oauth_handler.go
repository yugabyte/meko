package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// This file holds the provider-agnostic core for app-driven social sign-in
// (Cognito federation without the Hosted UI). Each provider (see
// github_auth_handler.go, google_auth_handler.go) supplies an oauthProvider
// value and a pair of thin handlers that delegate here.

// oauthStateCookieName is the short-lived, HttpOnly cookie that carries the
// CSRF state token between the login handler and the callback handler,
// which verifies and clears it.
const oauthStateCookieName = "oauth_state"

// oauthPKCECookieName is the short-lived, HttpOnly cookie that carries the
// PKCE code verifier between the login handler (which sends its SHA-256
// code_challenge to /oauth2/authorize) and the callback handler (which
// replays the plaintext verifier to /oauth2/token). Cognito's Managed
// Login requires PKCE for public app clients (those without a secret).
const oauthPKCECookieName = "oauth_pkce"

// oauthStartCookieMaxAge is how long the OAuth start cookies (state + PKCE
// verifier) live. They only need to survive the redirect round-trip to the
// provider. 15 minutes comfortably covers a user lingering on the consent
// screen while keeping the single-use CSRF/PKCE state short-lived; a longer
// pause fails cleanly (the callback finds no state cookie and bounces the
// user back to sign-in) — and would fail anyway, since the provider's own
// authorization code/request expires well before then.
const oauthStartCookieMaxAge = 15 * 60

// oauthProvider holds the per-provider knobs the shared social-login core
// needs. Adding a new social IdP (GitHub, Google, GitLab, ...) is just
// another value of this struct plus a pair of thin handlers.
type oauthProvider struct {
	// name is the Cognito identity_provider value; it must match the IdP
	// configured on the user pool (e.g. "GitHub", "Google").
	name string
	// callbackPath is the app route Cognito redirects back to. It must be
	// registered as an Allowed callback URL on the app client (origin + this
	// path).
	callbackPath string
	// reasonPrefix namespaces the ?error=<reason> codes the SPA maps,
	// e.g. "github" -> github_state / github_no_code / github_exchange.
	reasonPrefix string
	// onNewAccount is an optional hook that runs once, only when a brand-new
	// local account is JIT-provisioned in this callback. It's left nil today
	// (the name-confirmation step is driven entirely from server state via
	// /auth/me, not a callback-set cookie) but kept as the extension point
	// for any future provider that needs first-signup side effects.
	onNewAccount func(c echo.Context)
}

// startOAuthLogin begins an app-driven social sign-in: it stamps a CSRF
// state cookie and 302-redirects to Cognito's /oauth2/authorize with
// identity_provider set, which forwards straight to the provider — no
// Hosted UI login screen.
func startOAuthLogin(c echo.Context, cognitoDomain, clientID string, p oauthProvider) error {
	if cognitoDomain == "" || clientID == "" {
		return apiError(c, http.StatusServiceUnavailable,
			strings.ToUpper(p.reasonPrefix)+"_LOGIN_UNAVAILABLE",
			fmt.Sprintf("%s sign-in is not configured", p.name))
	}
	state, err := randomOAuthState()
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", fmt.Sprintf("failed to start %s sign-in", p.name))
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", fmt.Sprintf("failed to start %s sign-in", p.name))
	}
	setOAuthStateCookie(c, state)
	setOAuthPKCECookie(c, verifier)
	redirectURI := requestOrigin(c) + p.callbackPath
	return c.Redirect(http.StatusFound,
		buildAuthorizeURL(cognitoDomain, clientID, p.name, redirectURI, state, challenge))
}

// completeOAuthCallback finishes an app-driven social sign-in. Cognito
// redirects the browser here with ?code=&state=; we verify the CSRF state
// (mandatory and single-use — no soft-fail), exchange the code for tokens,
// JIT-provision the local account, set the same HttpOnly auth cookies as
// password login, and 302 into the SPA.
func completeOAuthCallback(
	c echo.Context,
	cognito *auth.CognitoClient,
	svc *service.AuthService,
	cognitoDomain string,
	p oauthProvider,
) error {
	cookie, err := c.Cookie(oauthStateCookieName)
	if err != nil || cookie.Value == "" {
		slog.Warn(p.name + " callback: missing state cookie, rejecting")
		return c.Redirect(http.StatusFound, appSignInRedirect(c, p.reasonPrefix+"_state"))
	}
	clearOAuthStateCookie(c)
	if qpState := c.QueryParam("state"); qpState == "" || qpState != cookie.Value {
		slog.Warn(p.name + " callback: state mismatch, rejecting")
		return c.Redirect(http.StatusFound, appSignInRedirect(c, p.reasonPrefix+"_state"))
	}

	if errParam := c.QueryParam("error"); errParam != "" {
		slog.Warn(p.name+" callback: provider returned error",
			"error", errParam,
			"error_description", c.QueryParam("error_description"),
			"error_uri", c.QueryParam("error_uri"),
		)
		return c.Redirect(http.StatusFound, appSignInRedirect(c, errParam))
	}

	code := c.QueryParam("code")
	if code == "" {
		return c.Redirect(http.StatusFound, appSignInRedirect(c, p.reasonPrefix+"_no_code"))
	}

	var codeVerifier string
	if pkce, pkceErr := c.Cookie(oauthPKCECookieName); pkceErr == nil {
		codeVerifier = pkce.Value
	}
	clearOAuthPKCECookie(c)

	exchangeRedirectURI := requestOrigin(c) + p.callbackPath
	result, err := cognito.ExchangeCode(
		c.Request().Context(), code, cognitoDomain, exchangeRedirectURI, codeVerifier,
	)
	if err != nil {
		slog.Error(p.name+" callback: code exchange failed", "error", err)
		return c.Redirect(http.StatusFound, appSignInRedirect(c, p.reasonPrefix+"_exchange"))
	}

	created, err := svc.EnsureAccountFromIDToken(c.Request().Context(), result.IDToken)
	if err != nil {
		// Don't set auth cookies on a half-provisioned account: that would
		// land the user in the SPA with a session but no local account, and
		// every subsequent API call would 401. Bounce back to sign-in.
		slog.Error(p.name+" callback: local account provisioning failed", "error", err)
		return c.Redirect(http.StatusFound, appSignInRedirect(c, p.reasonPrefix+"_provision"))
	}

	setAuthCookie(c, "access_token", result.AccessToken)
	setAuthCookie(c, "id_token", result.IDToken)
	setAuthCookie(c, "refresh_token", result.RefreshToken)

	if created && p.onNewAccount != nil {
		p.onNewAccount(c)
	}

	return c.Redirect(http.StatusFound, appHomeRedirect(c))
}

// buildAuthorizeURL builds the Cognito /oauth2/authorize URL that forwards
// directly to the given identity provider. identity_provider is what makes
// Cognito skip the Hosted UI login screen; it must match the IdP name
// configured on the user pool. codeChallenge is the PKCE S256 challenge;
// when non-empty it (and code_challenge_method=S256) are added so the
// callback can complete the exchange with the matching code_verifier.
func buildAuthorizeURL(cognitoDomain, clientID, provider, redirectURI, state, codeChallenge string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		// aws.cognito.signin.user.admin lets the resulting access token call
		// Cognito self-service attribute APIs (UpdateUserAttributes,
		// GetUserAttributeVerificationCode, VerifyUserAttribute) — required by
		// the post-signup email-collection/OTP flow for federated users who
		// arrive without an email. Without it those calls 401 with
		// "Access Token does not have required scopes". The app client's allowed
		// OAuth scopes MUST include this scope or Cognito rejects the authorize
		// request with invalid_scope.
		"scope":             {"openid email profile aws.cognito.signin.user.admin"},
		"identity_provider": {provider},
		"state":             {state},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	domain := strings.TrimRight(cognitoDomain, "/")
	return fmt.Sprintf("https://%s/oauth2/authorize?%s", domain, q.Encode())
}

// requestOrigin reconstructs scheme://host from the incoming request,
// honoring X-Forwarded-Proto and X-Forwarded-Host so it works behind
// the K8s ingress (which rewrites Host to the internal service name).
//
// X-Forwarded-Host may be a comma-separated list when the request has
// passed through multiple proxies (e.g. "app.mekodev.com, proxy1.example.com").
// Only the first entry is used to avoid malformed redirect URIs.
//
// SECURITY: ensure the ingress/reverse-proxy strips or overwrites
// X-Forwarded-Host from untrusted clients to prevent Host Header
// Injection / OAuth redirect-URI hijacking.
func requestOrigin(c echo.Context) string {
	scheme := c.Scheme()
	if fwd := strings.TrimSpace(strings.SplitN(c.Request().Header.Get("X-Forwarded-Proto"), ",", 2)[0]); fwd != "" {
		scheme = fwd
	}
	host := c.Request().Host
	if fwdHost := strings.TrimSpace(strings.SplitN(c.Request().Header.Get("X-Forwarded-Host"), ",", 2)[0]); fwdHost != "" {
		// Use only the first entry in a potentially comma-separated list.
		host = fwdHost
	}
	return scheme + "://" + host
}

// isSecure reports whether the original client request was HTTPS, honoring
// X-Forwarded-Proto. Behind a TLS-terminating reverse proxy (e.g. nginx) the
// hop to the Go server is plain HTTP, so c.Scheme() alone would mark cookies
// non-Secure; consulting the forwarded header keeps auth/CSRF/PKCE cookies
// flagged Secure in production.
func isSecure(c echo.Context) bool {
	if fwd := c.Request().Header.Get("X-Forwarded-Proto"); fwd != "" {
		return strings.EqualFold(fwd, "https")
	}
	return c.Scheme() == "https"
}

// appHomeRedirect points the browser at the SPA root after a successful
// sign-in; the SPA forwards to onboarding/datapacks based on status.
func appHomeRedirect(c echo.Context) string {
	return requestOrigin(c) + "/"
}

// appSignInRedirect sends the browser back to the SPA sign-in page with a
// short error code so the UI can surface a message.
func appSignInRedirect(c echo.Context, reason string) string {
	u := requestOrigin(c) + "/signin"
	if reason != "" {
		u += "?error=" + url.QueryEscape(reason)
	}
	return u
}

// setOAuthStateCookie stores the CSRF state in a short-lived HttpOnly
// cookie. Lax SameSite is required so the cookie survives the cross-site
// redirect back from the provider/Cognito.
func setOAuthStateCookie(c echo.Context, state string) {
	c.SetCookie(&http.Cookie{
		Name:     oauthStateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   oauthStartCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(c),
	})
}

// clearOAuthStateCookie expires the CSRF state cookie once consumed.
func clearOAuthStateCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(c),
	})
}

// randomOAuthState returns a 256-bit hex-encoded random string used as
// the OAuth2 CSRF state parameter.
func randomOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generatePKCE creates a PKCE code verifier and its S256 code challenge.
// The verifier is a high-entropy URL-safe string (RFC 7636 allows 43-128
// chars); the challenge is base64url(SHA-256(verifier)) without padding.
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// setOAuthPKCECookie stores the PKCE verifier in a short-lived HttpOnly
// cookie. Lax SameSite mirrors the state cookie so it survives the
// cross-site redirect back from the provider/Cognito.
func setOAuthPKCECookie(c echo.Context, verifier string) {
	c.SetCookie(&http.Cookie{
		Name:     oauthPKCECookieName,
		Value:    verifier,
		Path:     "/",
		MaxAge:   oauthStartCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(c),
	})
}

// clearOAuthPKCECookie expires the PKCE verifier cookie once consumed.
func clearOAuthPKCECookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     oauthPKCECookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(c),
	})
}
