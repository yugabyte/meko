package handlers

import (
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// linkedinProvider configures the shared social-login core for LinkedIn.
//
// LinkedIn is wired into Cognito as a generic OIDC provider (issuer
// https://www.linkedin.com/oauth) requesting the openid/profile/email
// scopes. Unlike GitHub (single free-text name), LinkedIn reliably returns
// structured given_name/family_name, so — like Google — we don't hint the
// name-confirmation modal via a cookie: the SPA decides purely from server
// state (`/auth/me`), so onNewAccount stays nil.
var linkedinProvider = oauthProvider{
	name:         "LinkedIn",
	callbackPath: "/api/v1/auth/linkedin/callback",
	reasonPrefix: "linkedin",
}

// LinkedinLoginConfig holds the Cognito coordinates needed to start a
// LinkedIn sign-in. Mirrors GoogleLoginConfig.
type LinkedinLoginConfig struct {
	// CognitoDomain is the bare Cognito domain (e.g. auth.mekodev.com).
	CognitoDomain string
	// ClientID is the Cognito App Client ID.
	ClientID string
}

// LinkedinLoginHandler starts the app-driven LinkedIn sign-in. The browser
// hits this endpoint and is 302-redirected to Cognito's /oauth2/authorize
// with identity_provider=LinkedIn, which forwards straight to LinkedIn — no
// Hosted UI login screen. The callback lands on /auth/linkedin/callback.
func LinkedinLoginHandler(cfg LinkedinLoginConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		return startOAuthLogin(c, cfg.CognitoDomain, cfg.ClientID, linkedinProvider)
	}
}

// LinkedinCallbackConfig holds the dependencies for the LinkedIn OAuth
// callback. Mirrors GoogleCallbackConfig.
type LinkedinCallbackConfig struct {
	CognitoClient *auth.CognitoClient
	AuthService   *service.AuthService
	CognitoDomain string
}

// LinkedinCallbackHandler completes the app-driven LinkedIn sign-in:
// verifies the CSRF state, exchanges the code for tokens, JIT-provisions
// the local account, sets the auth cookies, and 302s into the SPA. The SPA
// decides whether to show the one-time name-confirmation modal purely from
// server state (`/auth/me`) — no confirm-name cookie is set here.
func LinkedinCallbackHandler(cfg LinkedinCallbackConfig) echo.HandlerFunc {
	return func(c echo.Context) error {
		return completeOAuthCallback(
			c, cfg.CognitoClient, cfg.AuthService, cfg.CognitoDomain, linkedinProvider,
		)
	}
}
