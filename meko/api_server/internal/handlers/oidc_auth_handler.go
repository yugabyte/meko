package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// Browser sign-in against a generic OIDC issuer.
//
// The same shape as the Cognito-brokered social handlers beside this file —
// state cookie, PKCE, code exchange, JIT account, auth cookies, redirect into
// the SPA — and it reuses their helpers rather than restating them. What it
// does not have is a provider name: with a generic issuer there is exactly one
// login route, and which upstream IdP sits behind it (Google, LinkedIn, LDAP,
// SAML) is the issuer's configuration, not ours.

// oidcReason namespaces the ?error=<reason> codes the SPA maps, matching the
// convention the social callbacks use.
const oidcReason = "oidc"

// OIDCLoginConfig carries the discovered provider into the login handler.
type OIDCLoginConfig struct {
	Provider *auth.OIDCProvider
}

// OIDCLoginHandler serves GET /auth/oidc/login: stamp a CSRF state and a PKCE
// verifier, then 302 to the issuer's authorization endpoint.
func OIDCLoginHandler(cfg OIDCLoginConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		if cfg.Provider == nil {
			return apiError(c, http.StatusServiceUnavailable,
				"OIDC_LOGIN_UNAVAILABLE", "OIDC sign-in is not configured")
		}
		state, err := randomOAuthState()
		if err != nil {
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to start sign-in")
		}
		verifier, _, err := generatePKCE()
		if err != nil {
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to start sign-in")
		}
		nonce, err := randomOAuthState()
		if err != nil {
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to start sign-in")
		}
		// The challenge is derived inside AuthCodeURL from the verifier, so the
		// pair cannot disagree; generatePKCE's own challenge is discarded.
		authURL, err := cfg.Provider.AuthCodeURL(c.Request().Context(), state, verifier, nonce)
		if err != nil {
			slog.Error("oidc login: issuer unreachable", "error", err)
			return apiError(c, http.StatusServiceUnavailable,
				"OIDC_LOGIN_UNAVAILABLE", "the identity provider is not reachable right now")
		}
		setOAuthStateCookie(c, state)
		setOAuthPKCECookie(c, verifier)
		setOIDCNonceCookie(c, nonce)
		return c.Redirect(http.StatusFound, authURL)
	}
}

// OIDCCallbackConfig carries what the callback needs to finish a sign-in.
type OIDCCallbackConfig struct {
	Provider *auth.OIDCProvider
	// AuthService provisions the local account from the verified ID token. Only
	// EnsureAccountFromIDToken is used, which reads `sub` and nothing
	// Cognito-specific, so it serves both providers unchanged.
	AuthService *service.AuthService
}

// OIDCCallbackHandler serves GET /auth/oidc/callback.
//
// State is mandatory and single-use — a missing or mismatched state is a
// rejection, never a soft-fail — as is the nonce the issuer must echo into the
// id_token. The local account is provisioned BEFORE any auth cookie is set, so a
// half-provisioned user never reaches the SPA with a session that 401s on every
// call.
func OIDCCallbackHandler(cfg OIDCCallbackConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		if cfg.Provider == nil {
			return apiError(c, http.StatusServiceUnavailable,
				"OIDC_LOGIN_UNAVAILABLE", "OIDC sign-in is not configured")
		}

		cookie, err := c.Cookie(oauthStateCookieName)
		if err != nil || cookie.Value == "" {
			slog.Warn("oidc callback: missing state cookie, rejecting")
			return c.Redirect(http.StatusFound, appSignInRedirect(c, oidcReason+"_state"))
		}
		clearOAuthStateCookie(c)
		if qp := c.QueryParam("state"); qp == "" || qp != cookie.Value {
			slog.Warn("oidc callback: state mismatch, rejecting")
			return c.Redirect(http.StatusFound, appSignInRedirect(c, oidcReason+"_state"))
		}

		if errParam := c.QueryParam("error"); errParam != "" {
			slog.Warn("oidc callback: issuer returned error",
				"error", errParam,
				"error_description", c.QueryParam("error_description"),
			)
			return c.Redirect(http.StatusFound, appSignInRedirect(c, errParam))
		}

		code := c.QueryParam("code")
		if code == "" {
			return c.Redirect(http.StatusFound, appSignInRedirect(c, oidcReason+"_no_code"))
		}

		var verifier string
		if pkce, pkceErr := c.Cookie(oauthPKCECookieName); pkceErr == nil {
			verifier = pkce.Value
		}
		clearOAuthPKCECookie(c)
		var nonce string
		if ck, nonceErr := c.Cookie(oauthNonceCookieName); nonceErr == nil {
			nonce = ck.Value
		}
		clearOIDCNonceCookie(c)

		result, err := cfg.Provider.Exchange(c.Request().Context(), code, verifier, nonce)
		if err != nil {
			slog.Error("oidc callback: code exchange failed", "error", err)
			reason := oidcReason + "_exchange"
			if errors.Is(err, auth.ErrOIDCUnavailable) {
				reason = oidcReason + "_unavailable"
			}
			return c.Redirect(http.StatusFound, appSignInRedirect(c, reason))
		}

		if _, err := cfg.AuthService.EnsureAccountFromIDToken(
			c.Request().Context(), result.IDToken,
		); err != nil {
			slog.Error("oidc callback: local account provisioning failed", "error", err)
			return c.Redirect(http.StatusFound, appSignInRedirect(c, oidcReason+"_provision"))
		}

		// AccessToken and IDToken are the same verified ID token — see
		// OIDCProvider.Exchange for why. The cookie names are unchanged, so the
		// SPA and the auth middleware need no knowledge of which provider is on.
		setAuthCookie(c, "access_token", result.AccessToken)
		setAuthCookie(c, "id_token", result.IDToken)
		setAuthCookie(c, "refresh_token", result.RefreshToken)

		return c.Redirect(http.StatusFound, appHomeRedirect(c))
	}
}

// OIDCLogoutConfig carries what browser-driven sign-out needs.
type OIDCLogoutConfig struct {
	Provider *auth.OIDCProvider
}

// OIDCLogoutHandler serves GET /auth/logout for an OIDC deployment: clear this
// server's cookies, then hand the browser to the issuer's RP-initiated logout
// so its own session cookie goes too. Without that second half the next sign-in
// is silent and reuses the previous identity — the same trap the Cognito
// federated logout exists to avoid.
//
// end_session_endpoint is optional in the spec, so an issuer that publishes none
// gets the local half only, and the redirect goes straight to the SPA.
func OIDCLogoutHandler(cfg OIDCLogoutConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		var idTokenHint string
		if ck, err := c.Cookie("id_token"); err == nil {
			idTokenHint = ck.Value
		}
		clearAuthCookie(c, "access_token")
		clearAuthCookie(c, "id_token")
		clearAuthCookie(c, "refresh_token")

		home := appHomeRedirect(c)
		if cfg.Provider == nil {
			return c.Redirect(http.StatusFound, home)
		}
		if u := cfg.Provider.EndSessionURL(idTokenHint, requestOrigin(c)); u != "" {
			return c.Redirect(http.StatusFound, u)
		}
		slog.Debug("oidc logout: issuer publishes no end_session_endpoint, local sign-out only")
		return c.Redirect(http.StatusFound, home)
	}
}
