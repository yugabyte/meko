package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// AuthConfig tells the SPA which identity provider is live, so the sign-in page
// renders what this deployment can actually do rather than a fixed set of forms.
//
// It is unauthenticated by necessity — it is read before anyone can sign in —
// and says nothing secret: which provider, where to start a login, and which
// affordances exist. Client IDs, issuer URLs and domains stay out of it; the
// browser never needs them, because every flow starts at this server.
type AuthConfig struct {
	// Provider is "cognito", "oidc", or "none" (dev auth, every request sharing
	// one account).
	Provider string `json:"provider"`
	// LoginURL is where the sign-in button sends the browser, for a provider
	// whose flow is a redirect. Empty for cognito, whose sign-in is a form this
	// server posts to.
	LoginURL string `json:"login_url,omitempty"`
	// PasswordLogin and Signup are false for OIDC: the issuer owns its users, so
	// there is no password form and no signup page to route to.
	PasswordLogin bool `json:"password_login"`
	Signup        bool `json:"signup"`
	// SocialProviders are the Cognito-brokered buttons. Empty under OIDC, where
	// federation is the issuer's configuration and invisible from here.
	SocialProviders []string `json:"social_providers"`
}

// AuthConfigHandler serves GET /auth/config.
//
//	@Summary		Identity provider configuration
//	@Description	Which identity provider this deployment authenticates against, and the sign-in affordances it offers. Unauthenticated: the SPA reads it before anyone can sign in.
//	@Tags			auth
//	@Produce		json
//	@Success		200	{object}	APIResponse{data=AuthConfig}
//	@Router			/auth/config [get]
func AuthConfigHandler(cfg AuthConfig) echo.HandlerFunc {
	if cfg.SocialProviders == nil {
		// Render [] rather than null: the SPA maps over it.
		cfg.SocialProviders = []string{}
	}
	return func(c echo.Context) error {
		// Enveloped like every other JSON route, so the SPA reads it through the
		// same api client rather than a fetch of its own.
		return ok(c, http.StatusOK, cfg)
	}
}
