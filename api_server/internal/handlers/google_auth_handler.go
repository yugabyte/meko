package handlers

import (
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// googleProvider configures the shared social-login core for Google.
//
// Google is a native OIDC provider in Cognito (no OIDC proxy needed) and
// returns structured given_name/family_name claims. Those names are still
// not guaranteed though — last name is optional on a Google account and
// may be absent entirely. We no longer hint the name-confirmation modal via
// a cookie: the SPA decides purely from server state (`/auth/me` first/last
// name), so onNewAccount stays nil.
var googleProvider = oauthProvider{
	name:         "Google",
	callbackPath: "/api/v1/auth/google/callback",
	reasonPrefix: "google",
}

// GoogleLoginConfig holds the Cognito coordinates needed to start a Google
// sign-in. Mirrors GithubLoginConfig.
type GoogleLoginConfig struct {
	// CognitoDomain is the bare Cognito domain (e.g. auth.mekodev.com).
	CognitoDomain string
	// ClientID is the Cognito App Client ID.
	ClientID string
}

// GoogleLoginHandler starts the app-driven Google sign-in. The browser
// hits this endpoint and is 302-redirected to Cognito's /oauth2/authorize
// with identity_provider=Google, which forwards straight to Google — no
// Hosted UI login screen. The callback lands on /auth/google/callback.
func GoogleLoginHandler(cfg GoogleLoginConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		return startOAuthLogin(c, cfg.CognitoDomain, cfg.ClientID, googleProvider)
	}
}

// GoogleCallbackConfig holds the dependencies for the Google OAuth
// callback. Mirrors GithubCallbackConfig.
type GoogleCallbackConfig struct {
	CognitoClient *auth.CognitoClient
	AuthService   *service.AuthService
	CognitoDomain string
}

// GoogleCallbackHandler completes the app-driven Google sign-in: verifies
// the CSRF state, exchanges the code for tokens, JIT-provisions the local
// account, sets the auth cookies, and 302s into the SPA. The SPA decides
// whether to show the one-time name-confirmation modal purely from server
// state (`/auth/me` first/last name) — no confirm-name cookie is set here
// (last name is optional on Google accounts, so the name may be incomplete).
func GoogleCallbackHandler(cfg GoogleCallbackConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		return completeOAuthCallback(
			c, cfg.CognitoClient, cfg.AuthService, cfg.CognitoDomain, googleProvider,
		)
	}
}
