package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/service"
)

// registerOIDCAuthRoutes wires authentication against a generic OIDC issuer.
//
// The contrast with registerAuthRoutes is the point of this file: the routes
// below are all of them. There is no signup, no confirm-signup, no
// forgot-password, no change-password and no email-verification endpoint,
// because an OIDC issuer owns its users and does those on its own pages. A
// request to one of them 404s rather than reaching a handler that would have to
// explain itself.
//
// When Cognito is dropped, this becomes the only provider and registerAuthRoutes
// is deleted whole: nothing here imports it, and the pieces both use — the PAT
// adapter, the /auth/me surface, AuthService's account provisioning — already
// live outside it.
func registerOIDCAuthRoutes(
	cfg ServerConfig,
	v1 *echo.Group,
	datapackGroup **echo.Group,
	dpDirect **echo.Group,
	reg *serviceRegistry,
) error {
	if cfg.OIDCRedirectURL == "" {
		return fmt.Errorf(
			"auth provider %q needs OIDCRedirectURL: it must match the URI registered "+
				"with the issuer byte-for-byte and cannot be derived behind a proxy",
			authProviderOIDC,
		)
	}
	// Construction does not reach the issuer: NewOIDCProvider validates the
	// config and retries discovery on demand, so an issuer that is down at boot
	// costs sign-in (503) rather than the whole api-server, including the PAT and
	// MCP traffic that never touches an IdP.
	provider, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
		IssuerURL:    cfg.OIDCIssuerURL,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		RedirectURL:  cfg.OIDCRedirectURL,
		Scopes:       cfg.OIDCScopes,
	})
	if err != nil {
		return fmt.Errorf("initializing oidc provider: %w", err)
	}

	var authOpts []service.AuthServiceOption
	if provisioner := buildProvisioner(cfg, reg); provisioner != nil {
		authOpts = append(authOpts, service.WithProvisioner(provisioner))
	}
	if reg.sharingRepo != nil {
		authOpts = append(authOpts, service.WithInviteBackfiller(reg.sharingRepo))
	}
	// The issuer verified the address and owns the user, and there is no
	// email-verification route on this path to finish with.
	authOpts = append(authOpts, service.WithIssuerOwnedEmail())
	// No SetCognito on the sharing service: inviting someone who has no account
	// yet would mean creating a user, which is the issuer's to do. SharingService
	// already nil-checks it and degrades to inviting existing users only.
	authSvc := service.NewAuthService(service.NoUserStore{}, reg.accountRepo, authOpts...)
	authH := handlers.NewAuthHandler(authSvc, reg.accountUsageSvc)

	validator := auth.NewOIDCValidator(provider)
	requireAuth := auth.RequireAuth(validator, reg.accountRepo)
	requireAuthWithPAT := auth.RequireAuth(
		validator,
		reg.accountRepo,
		auth.WithPAT(newPATAdapter(reg)),
	)

	authGroup := v1.Group("/auth")
	authGroup.GET("/oidc/login", handlers.OIDCLoginHandler(handlers.OIDCLoginConfig{
		Provider: provider,
	}))
	authGroup.GET("/oidc/callback", handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
		Provider:    provider,
		AuthService: authSvc,
	}))
	// Browser-driven sign-out, the same route the Cognito path serves, so the
	// SPA's sign-out link is provider-independent.
	authGroup.GET("/logout", handlers.OIDCLogoutHandler(handlers.OIDCLogoutConfig{
		Provider: provider,
	}))
	registerSessionRoutes(authGroup, authH, requireAuth)

	*datapackGroup = v1.Group("", requireAuth)
	*dpDirect = v1.Group("",
		requireAuthWithPAT,
		PerPATRateLimiter(cfg.PATRateLimitPerMin, cfg.PATRateLimitBurst),
	)

	slog.Info("oidc auth routes registered",
		"issuer", cfg.OIDCIssuerURL,
		"redirect_url", cfg.OIDCRedirectURL,
		"pat_rate_limit_per_min", cfg.PATRateLimitPerMin,
	)
	return nil
}
