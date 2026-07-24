package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "meko/api_server/docs" // registers swagger spec with swag
	"meko/api_server/internal/auth"
	"meko/api_server/internal/email"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/repository"
	adminrepo "meko/api_server/internal/repository/admin"
	"meko/api_server/internal/service"
	adminsvc "meko/api_server/internal/service/admin"
	"meko/api_server/internal/yba"
)

// serviceRegistry holds every repository and service the router needs,
// built once and shared across all route-registration helpers.
type serviceRegistry struct {
	accountRepo *repository.AccountRepo
	ybaRepo     *repository.YBARepo
	ybaSvc      *service.YBAService
	dpRepo      *repository.DatapackRepo
	dpSvc       *service.DatapackService
	// kbRepo: shared by kbSvc and the AccountUsageService (which is
	// what the /auth/me/usage handler depends on — handlers never touch
	// repos directly).
	kbRepo *repository.KnowledgeBaseRepo
	// freeTierRepo: read by AccountUsageService for /auth/me/usage's
	// num_conversations counter. Written by meko-mcp-server's quota gate.
	freeTierRepo       *repository.FreeTierUsageRepo
	accountUsageSvc    *service.AccountUsageService
	langfuseSvc        *service.LangfuseService
	agentSvc           *service.AgentService
	kbSvc              *service.KnowledgeBaseService
	ragSvc             *service.RagService
	memoriesSvc        *service.MemoriesService
	promoteMemoriesSvc *service.PromoteMemoriesService
	s3Svc              *service.S3Service
	sharingRepo        *repository.SharingRepo
	sharingSvc         *service.SharingService
	feedbackRepo       *repository.FeedbackRepo
	mcpPATRepo         *repository.MCPPATRepo
	mcpPATAuditRepo    *repository.MCPPATAuditRepo
	mcpPATSvc          *service.MCPPATService
	billingRepo        *repository.BillingRepo
	billingSvc         *service.BillingService
	adminMetricsSvc    *adminsvc.AdminMetricsService
	adminDataPlaneSvc  *adminsvc.AdminDataPlaneService
	artifactRepo       *repository.ArtifactRepo
	artifactSvc        *service.ArtifactService
	tokenSavingsRepo   *repository.TokenSavingsRepo
	tokenSavingsSvc    *service.TokenSavingsService
	// ampSvc is nil unless the AMP feature is enabled (--show-pg-amp); the AMP
	// datapack-database routes are registered only when it is non-nil. The AMP
	// controller endpoint is resolved at request time from the registration row,
	// not a flag.
	ampSvc *service.AMPService
	// ampRegistrationRepo backs the admin AMP-registration endpoints (managed
	// by meko_admin). Always present — registration is what enables AMP, so it
	// must be manageable independent of the --show-pg-amp datapack routes.
	ampRegistrationRepo *repository.AMPRegistrationRepo
}

// buildServiceRegistry initialises all repositories and services in
// dependency order so that every component shares a single instance.
func buildServiceRegistry(cfg ServerConfig) *serviceRegistry {
	slog.Info("initializing service registry")

	accountRepo := repository.NewAccountRepo(cfg.SystemDB)
	ybaRepo := repository.NewYBARepo(cfg.SystemDB)
	dpRepo := repository.NewDatapackRepo(cfg.SystemDB, cfg.MCPServerURL, cfg.DeployEnv)

	ybaSvc := service.NewYBAService(ybaRepo)

	agentRepo := repository.NewAgentRepo(cfg.SystemDB)
	agentSvc := service.NewAgentService(agentRepo)

	kbRepo := repository.NewKnowledgeBaseRepo(cfg.SystemDB, cfg.DB)
	kbSvc := service.NewKnowledgeBaseService(kbRepo, cfg.S3Bucket)
	freeTierRepo := repository.NewFreeTierUsageRepo(cfg.SystemDB)
	ragSvc := service.NewRagService(cfg.DB, service.DatapackIndexSchema, service.DatapackIndexName)

	memoriesRepo := repository.NewMemoriesRepo(
		cfg.DB,
		cfg.SystemDB,
		cfg.OpenAIAPIKey,
		cfg.OpenAILLMModel,
		cfg.OpenAIEmbeddingModel,
		cfg.MemoryListSkipVectorSort,
	)
	memoriesSvc := service.NewMemoriesService(memoriesRepo, cfg.MemoryListSkipVectorSort)

	promoteMemoriesRepo := repository.NewPromoteMemoriesRepo(cfg.DB, cfg.SystemDB)
	promoteMemoriesSvc := service.NewPromoteMemoriesService(promoteMemoriesRepo)

	sharingRepo := repository.NewSharingRepo(cfg.SystemDB)
	sharingSvc := service.NewSharingService(sharingRepo, nil)
	feedbackRepo := repository.NewFeedbackRepo(cfg.SystemDB)
	var s3Svc *service.S3Service
	if cfg.S3Bucket != "" {
		var err error
		s3Svc, err = service.NewS3Service(service.S3Config{
			AuthMode:  service.S3AuthMode(cfg.S3AuthMode),
			Endpoint:  cfg.S3Endpoint,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
			Bucket:    cfg.S3Bucket,
			Region:    cfg.S3Region,
		})
		if err != nil {
			slog.Error("failed to initialize S3 service", "error", err)
		}
	}

	langfuseSvc := service.NewLangfuseService(service.LangfuseConfig{
		SystemDB:    cfg.SystemDB,
		DataDB:      cfg.DB,
		LangfuseDB:  cfg.LangfuseDB,
		Host:        cfg.LangfuseHost,
		AdminAPIKey: cfg.LangfuseAdminAPIKey,
	})
	if !langfuseSvc.Enabled() {
		slog.Warn("langfuse integration disabled; set host + admin api key to enable")
	}
	sharingSvc.SetLangfuse(langfuseSvc)

	if cfg.SESFromAddress != "" {
		sesEmailer, sesErr := email.NewSESEmailer(context.Background(), email.SESConfig{
			Region:      cfg.SESRegion,
			FromAddress: cfg.SESFromAddress,
			AppURL:      cfg.AppURL,
		})
		if sesErr != nil {
			slog.Error("ses emailer init failed; invite emails disabled", "error", sesErr)
		} else {
			sharingSvc.SetEmailer(sesEmailer)
			slog.Info("ses emailer initialized", "from", cfg.SESFromAddress)
		}
	} else {
		slog.Warn("ses invite emails disabled (set SES_FROM_ADDRESS to enable)")
	}

	dpSvc := service.NewDatapackService(dpRepo, ybaSvc, accountRepo, langfuseSvc, freeTierRepo)

	mcpPATRepo := repository.NewMCPPATRepo(cfg.SystemDB)
	mcpPATAuditRepo := repository.NewMCPPATAuditRepo(cfg.SystemDB)
	mcpPATSvc := service.NewMCPPATService(mcpPATRepo, mcpPATAuditRepo)

	billingRepo := repository.NewBillingRepo(cfg.SystemDB)
	billingSvc := service.NewBillingService(service.BillingConfig{
		StripeSecretKey:     cfg.StripeSecretKey,
		StripeWebhookSecret: cfg.StripeWebhookSecret,
		PriceIDStarter:      cfg.StripePriceIDStarter,
		PriceIDGrowth:       cfg.StripePriceIDGrowth,
		PriceIDScale:        cfg.StripePriceIDScale,
		AppURL:              cfg.AppURL,
	}, billingRepo)
	if billingSvc.Enabled() {
		slog.Info("billing routes will be registered (stripe configured)")
	} else {
		slog.Warn("billing routes disabled (stripe secret/webhook/price ids not all set)")
	}

	accountUsageSvc := service.NewAccountUsageService(dpRepo, kbRepo, langfuseSvc.Repo(), freeTierRepo)

	// Admin metrics span all three data stores (control plane, data plane,
	// Langfuse) via a dedicated admin repo/service pair.
	adminRepo := adminrepo.NewAdminRepo(cfg.SystemDB, cfg.DB, cfg.LangfuseDB)
	adminMetricsSvc := adminsvc.NewAdminMetricsService(adminRepo)

	// Data Plane tab: reads live universe config/metrics from YBA. The YBA
	// client is nil when no token is set, in which case the endpoint reports
	// the data plane as not configured (no 503).
	ybaClient := yba.NewClient(cfg.YBAAPIToken)
	if ybaClient == nil {
		slog.Warn("admin dataplane: YBA integration disabled (set --yba-api-token to enable)")
	}
	adminDataPlaneSvc := adminsvc.NewAdminDataPlaneService(adminRepo, ybaClient)

	// AMP controller registration (singleton). Managed by meko_admin via the
	// admin API and always available — it's what enables AMP, so it can't be
	// gated behind --show-pg-amp.
	ampRegistrationRepo := repository.NewAMPRegistrationRepo(cfg.SystemDB)

	// AMP (Postgres databases) integration. Enabled only when the feature flag
	// is on; the controller endpoint is resolved from the database at runtime.
	var ampSvc *service.AMPService
	if cfg.ShowPGAMP {
		ampSvc = service.NewAMPService(
			repository.NewAMPRepo(cfg.SystemDB),
			ampRegistrationRepo,
		)
		slog.Info("amp routes enabled (controller endpoint resolved from amp_registration)")
	} else {
		slog.Warn("amp routes disabled (set --show-pg-amp to enable)")
	}

	artifactRepo := repository.NewArtifactRepo(cfg.DB)
	artifactSvc := service.NewArtifactService(artifactRepo, s3Svc)

	tokenSavingsRepo := repository.NewTokenSavingsRepo(cfg.SystemDB)
	tokenSavingsSvc := service.NewTokenSavingsService(tokenSavingsRepo, dpRepo)

	slog.Info("service registry initialized")
	return &serviceRegistry{
		accountRepo:         accountRepo,
		ybaRepo:             ybaRepo,
		ybaSvc:              ybaSvc,
		dpRepo:              dpRepo,
		dpSvc:               dpSvc,
		kbRepo:              kbRepo,
		freeTierRepo:        freeTierRepo,
		accountUsageSvc:     accountUsageSvc,
		langfuseSvc:         langfuseSvc,
		agentSvc:            agentSvc,
		kbSvc:               kbSvc,
		ragSvc:              ragSvc,
		memoriesSvc:         memoriesSvc,
		promoteMemoriesSvc:  promoteMemoriesSvc,
		s3Svc:               s3Svc,
		sharingRepo:         sharingRepo,
		sharingSvc:          sharingSvc,
		feedbackRepo:        feedbackRepo,
		mcpPATRepo:          mcpPATRepo,
		mcpPATAuditRepo:     mcpPATAuditRepo,
		mcpPATSvc:           mcpPATSvc,
		billingRepo:         billingRepo,
		billingSvc:          billingSvc,
		adminMetricsSvc:     adminMetricsSvc,
		adminDataPlaneSvc:   adminDataPlaneSvc,
		artifactRepo:        artifactRepo,
		artifactSvc:         artifactSvc,
		tokenSavingsRepo:    tokenSavingsRepo,
		tokenSavingsSvc:     tokenSavingsSvc,
		ampSvc:              ampSvc,
		ampRegistrationRepo: ampRegistrationRepo,
	}
}

// NewRouter builds an Echo instance with all API routes registered.
// When Cognito config is provided, auth routes are registered and
// datapack routes require a valid JWT. When Cognito config is empty
// (e.g. in tests), all routes are public.
//
// The constructed *serviceRegistry is also returned so the caller
// (api.Run) can wire long-lived background workers -- specifically
// the MCP PAT last_used_at flusher -- against the same repo
// instances that are serving requests.
func NewRouter(cfg ServerConfig) (*echo.Echo, *serviceRegistry, error) {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = handlers.CustomHTTPErrorHandler
	RegisterMiddleware(e)

	e.GET("/swagger/*", echoSwagger.WrapHandler)

	reg := buildServiceRegistry(cfg)

	var successCfg *handlers.SuccessConfig

	v1 := e.Group("/api/v1", ManagementRateLimiter())

	v1.GET("/health", handlers.HealthHandler(cfg.SystemDB, cfg.DB))

	// Internal machine-to-machine routes. Protected by a static
	// shared secret rather than the user-facing JWT path, so they
	// hang off v1 directly and not under datapackGroup. When the
	// secret is unset on cfg, the middleware short-circuits every
	// request with 503.
	internalGroup := v1.Group("/internal", auth.RequireInternalSecret(cfg.MCPInternalSecret))
	patIntH := handlers.NewMCPPATInternalHandler(reg.mcpPATSvc)
	internalGroup.POST("/meko-tokens/validate", patIntH.Validate)

	// Admin metrics routes consumed by meko_admin. Gated by the admin
	// shared secret (X-Meko-Admin-Secret); 503 when the secret is unset.
	adminGroup := v1.Group("/admin", auth.RequireAdminSecret(cfg.AdminAPIKey))
	adminH := handlers.NewAdminHandler(reg.adminMetricsSvc, reg.adminDataPlaneSvc)
	adminGroup.GET("/users", adminH.Users)
	adminGroup.GET("/metrics", adminH.Metrics)
	adminGroup.GET("/token-savings", adminH.Executive)
	adminGroup.GET("/dataplane", adminH.DataPlane)
	adminGroup.PATCH("/users/:id/tier", adminH.UpdateUserTier)

	// AMP controller registration, managed by meko_admin through api_server so
	// the write path shares api_server's system-DB connection.
	ampRegH := handlers.NewAMPRegistrationHandler(reg.ampRegistrationRepo)
	adminGroup.GET("/amp-registration", ampRegH.Get)
	adminGroup.POST("/amp-registration", ampRegH.Register)
	adminGroup.PATCH("/amp-registration", ampRegH.Patch)
	adminGroup.DELETE("/amp-registration", ampRegH.Delete)

	authEnabled := cfg.CognitoRegion != "" &&
		cfg.CognitoPoolID != "" &&
		cfg.CognitoClientID != ""

	datapackGroup := v1.Group("")
	// dpDirect is the only group that opts into PAT acceptance via
	// auth.WithPAT. The 7 DatapackHandler routes hang off it; every
	// other route stays JWT-only on datapackGroup. In dev mode (auth
	// disabled) the two groups are identical -- DevAuth synthesises
	// pat_scopes=["*"] so RequireScope is a no-op.
	dpDirect := datapackGroup

	if authEnabled {
		authDeps, err := registerAuthRoutes(cfg, v1, &datapackGroup, &dpDirect, reg)
		if err != nil {
			return nil, nil, err
		}
		if cfg.CognitoDomain != "" {
			successCfg = &handlers.SuccessConfig{
				CognitoClient: authDeps.cognitoClient,
				AuthService:   authDeps.authService,
				CognitoDomain: cfg.CognitoDomain,
			}
		}
	} else {
		slog.Warn("cognito config not set, auth disabled – using dev account for all requests")
		devAuth := auth.DevAuth(reg.accountRepo)
		datapackGroup = v1.Group("", devAuth)
		dpDirect = datapackGroup
		// Register the minimal /auth surface the SPA needs so it can bootstrap
		// (and complete onboarding) without Cognito. See registerNoAuthRoutes.
		registerNoAuthRoutes(v1, devAuth, reg)
	}

	e.GET("/success", handlers.SuccessHandler(successCfg))

	registerDatapackRoutes(datapackGroup, dpDirect, reg, cfg.MaxArtifactSizeBytes)
	registerBillingRoutes(v1, datapackGroup, reg)

	return e, reg, nil
}

// registerBillingRoutes wires the three Stripe-backed endpoints when
// BillingService is enabled. Skipped entirely when Stripe isn't
// configured -- a missing route surfaces the misconfiguration the
// first time the UI tries to upgrade instead of silently returning
// 503 forever.
//
// The webhook handler attaches to v1 directly (no auth middleware)
// because Stripe authenticates via the Stripe-Signature header; the
// two session endpoints attach to datapackGroup so they go through
// the same JWT path as the rest of the user-facing API.
func registerBillingRoutes(v1 *echo.Group, authed *echo.Group, reg *serviceRegistry) {
	if reg.billingSvc == nil || !reg.billingSvc.Enabled() {
		return
	}
	h := handlers.NewBillingHandler(reg.billingSvc)
	authed.GET("/billing/summary", h.Summary)
	authed.POST("/billing/checkout-session", h.CreateCheckoutSession)
	authed.POST("/billing/portal-session", h.CreatePortalSession)
	v1.POST("/billing/webhook", h.Webhook)
	slog.Info("billing routes registered")
}

// authRouteDeps holds the initialized auth objects needed by the
// success handler.
type authRouteDeps struct {
	cognitoClient *auth.CognitoClient
	authService   *service.AuthService
}

// registerAuthRoutes wires the public /auth endpoints and configures
// the datapackGroup to require JWT authentication. It also builds
// dpDirect, a sibling group that accepts EITHER a Cognito JWT or a
// meko PAT and is wrapped by the per-PAT rate limiter. dpDirect is
// the only group that opts into PAT acceptance; everything attached
// to datapackGroup remains JWT-only and 401s on a PAT bearer.
func registerAuthRoutes(
	cfg ServerConfig,
	v1 *echo.Group,
	datapackGroup **echo.Group,
	dpDirect **echo.Group,
	reg *serviceRegistry,
) (*authRouteDeps, error) {
	cognitoClient, err := auth.NewCognitoClient(
		context.Background(),
		cfg.CognitoRegion,
		cfg.CognitoPoolID,
		cfg.CognitoClientID,
		cfg.CognitoClientSecret,
	)
	if err != nil {
		return nil, fmt.Errorf("initializing cognito client: %w", err)
	}

	var authOpts []service.AuthServiceOption
	if provisioner := buildProvisioner(cfg, reg); provisioner != nil {
		authOpts = append(authOpts, service.WithProvisioner(provisioner))
	}
	if reg.sharingRepo != nil {
		authOpts = append(authOpts, service.WithInviteBackfiller(reg.sharingRepo))
	}

	// Late-bind the cognito client into the sharing service now that
	// it has been initialized.
	reg.sharingSvc.SetCognito(cognitoClient)

	authSvc := service.NewAuthService(cognitoClient, reg.accountRepo, authOpts...)
	authH := handlers.NewAuthHandler(authSvc, reg.accountUsageSvc)

	jwtValidator := auth.NewJWTValidator(
		cfg.CognitoRegion,
		cfg.CognitoPoolID,
		cfg.CognitoClientID,
	)
	// JWT-only middleware: every route under datapackGroup keeps
	// today's behaviour (PAT bearers fall through to the JWT
	// validator and 401 cleanly).
	requireAuth := auth.RequireAuth(jwtValidator, reg.accountRepo)

	// PAT-accepting middleware: only used by dpDirect below. The
	// adapter converts the auth-package interface to the service-
	// package call without leaking service types into auth (which
	// would create a service<->auth import cycle).
	patAdapter := auth.PATValidatorFunc(func(
		ctx context.Context,
		in auth.PATValidateInput,
	) (*auth.PATValidateResult, error) {
		resp, err := reg.mcpPATSvc.ValidatePAT(ctx, service.MCPPATValidateInput{
			Plaintext:      in.Plaintext,
			ActorIP:        in.ActorIP,
			ActorUserAgent: in.ActorUserAgent,
		})
		if err != nil {
			return nil, err
		}
		return &auth.PATValidateResult{
			Valid:   resp.Valid,
			TokenID: resp.TokenID,
			UserID:  resp.UserID,
			Scopes:  resp.Scopes,
			Reason:  string(resp.Reason),
		}, nil
	})
	requireAuthWithPAT := auth.RequireAuth(
		jwtValidator,
		reg.accountRepo,
		auth.WithPAT(patAdapter),
	)

	authGroup := v1.Group("/auth")
	authGroup.POST("/signup", authH.Signup)
	authGroup.POST("/confirm-signup", authH.ConfirmSignup)
	authGroup.POST("/resend-confirmation", authH.ResendConfirmation)
	authGroup.POST("/forgot-password", authH.ForgotPassword)
	authGroup.POST("/forgot-password/confirm", authH.ForgotPasswordConfirm)
	authGroup.POST("/login", authH.Login)
	authGroup.POST("/complete-new-password", authH.CompleteNewPassword)
	authGroup.POST("/logout", authH.Logout)
	// Browser-driven federated sign-out. Unlike POST /logout (token
	// revocation only), this 302s the browser through Cognito's /logout so
	// the hosted "cognito" session cookie is cleared — otherwise the next
	// login silently reuses the previous identity. Provider-agnostic: works
	// for native, Google, LinkedIn, and GitHub sessions.
	authGroup.GET("/logout", handlers.FederatedLogoutHandler(handlers.FederatedLogoutConfig{
		AuthService:   authSvc,
		CognitoDomain: cfg.CognitoDomain,
		ClientID:      cfg.CognitoClientID,
	}))
	// App-driven social sign-in. The browser hits /<provider>/login and we
	// redirect straight to the IdP via Cognito's /oauth2/authorize
	// (identity_provider=<Provider>) — no Hosted UI login screen. The OAuth2
	// callback lands on /<provider>/callback, which exchanges the code, sets
	// the auth cookies, and redirects into the SPA. Requires the Cognito
	// domain. LinkedIn is wired into Cognito as a generic OIDC provider
	// (issuer https://www.linkedin.com/oauth); Google is a native provider.
	if cfg.CognitoDomain != "" {
		authGroup.GET("/google/login", handlers.GoogleLoginHandler(handlers.GoogleLoginConfig{
			CognitoDomain: cfg.CognitoDomain,
			ClientID:      cfg.CognitoClientID,
		}))
		authGroup.GET("/google/callback", handlers.GoogleCallbackHandler(handlers.GoogleCallbackConfig{
			CognitoClient: cognitoClient,
			AuthService:   authSvc,
			CognitoDomain: cfg.CognitoDomain,
		}))
		authGroup.GET("/linkedin/login", handlers.LinkedinLoginHandler(handlers.LinkedinLoginConfig{
			CognitoDomain: cfg.CognitoDomain,
			ClientID:      cfg.CognitoClientID,
		}))
		authGroup.GET("/linkedin/callback", handlers.LinkedinCallbackHandler(handlers.LinkedinCallbackConfig{
			CognitoClient: cognitoClient,
			AuthService:   authSvc,
			CognitoDomain: cfg.CognitoDomain,
		}))
	}
	// Change password is gated by requireAuth so the access-token cookie
	// is guaranteed present when the handler runs; Cognito then verifies
	// the previous-password against the user resolved from that token.
	authGroup.POST("/change-password", authH.ChangePassword, requireAuth)
	authGroup.GET("/me", authH.Me, requireAuth)
	authGroup.PATCH("/me", authH.UpdateOnboardingStatus, requireAuth)
	authGroup.PATCH("/me/profile", authH.UpdateProfile, requireAuth)
	// Post-signup email collection/verification for federated (LinkedIn) users
	// whose IdP sent no email (they carry a placeholder until they verify a
	// real address). All require a signed-in access token.
	authGroup.POST("/me/email/start", authH.StartEmailChange, requireAuth)
	authGroup.POST("/me/email/resend", authH.ResendEmailChangeCode, requireAuth)
	authGroup.POST("/me/email/verify", authH.VerifyEmailChange, requireAuth)
	// Free-tier usage counters; see AuthHandler.GetUsage for shape.
	authGroup.GET("/me/usage", authH.GetUsage, requireAuth)

	*datapackGroup = v1.Group("", requireAuth)
	*dpDirect = v1.Group("",
		requireAuthWithPAT,
		PerPATRateLimiter(cfg.PATRateLimitPerMin, cfg.PATRateLimitBurst),
	)

	slog.Info("auth routes registered",
		"pat_rate_limit_per_min", cfg.PATRateLimitPerMin,
		"pat_rate_limit_burst", cfg.PATRateLimitBurst,
	)
	return &authRouteDeps{
		cognitoClient: cognitoClient,
		authService:   authSvc,
	}, nil
}

// registerNoAuthRoutes wires the minimal /auth surface the SPA needs when
// Cognito is disabled (auth off). The web UI bootstraps by probing
// GET /auth/me; without it the SPA treats the session as expired and hard-gates
// to the login screen. These routes are all backed by the DevAuth single-user
// account and only read the account from context / the account repo / the usage
// service — never Cognito — so a nil Cognito provider is safe here. The
// Cognito-only routes (signup, login, forgot-password, social, change-password)
// are intentionally absent: DevAuth authenticates every request, so there is
// nothing to log into.
func registerNoAuthRoutes(v1 *echo.Group, devAuth echo.MiddlewareFunc, reg *serviceRegistry) {
	authSvc := service.NewAuthService(nil, reg.accountRepo)
	authH := handlers.NewAuthHandler(authSvc, reg.accountUsageSvc)

	g := v1.Group("/auth", devAuth)
	g.GET("/me", authH.Me)
	g.PATCH("/me", authH.UpdateOnboardingStatus)
	g.PATCH("/me/profile", authH.UpdateProfile)
	g.GET("/me/usage", authH.GetUsage)
	// No real session exists in no-auth mode (DevAuth needs no token), so logout
	// just acknowledges; the SPA re-bootstraps straight back in via DevAuth.
	logout := func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"message": "logged out"})
	}
	g.POST("/logout", logout)
	g.GET("/logout", logout)

	slog.Info("auth disabled: registered dev /auth/me routes (no Cognito)")
}

// buildProvisioner builds the provisioner used after signup. In the
// multi-tenant model the default datapack + agent + link rows are
// inserted in a single transaction by DatapackService.Create; there is
// no K8s CRD or per-datapack DB to apply, so the provisioner is just a
// thin wrapper around the datapack service plus best-effort Langfuse
// provisioning.
func buildProvisioner(_ ServerConfig, reg *serviceRegistry) service.Provisioner {
	return service.NewProvisioningService(
		reg.dpSvc,
		reg.accountRepo,
		reg.langfuseSvc,
	)
}

// registerDatapackRoutes wires all datapack, observe, agent, and
// knowledge-base routes onto the given groups.
//
// g is the JWT-only group used by everything except the 7
// DatapackHandler endpoints that the MCP server / cli_app need to
// reach with a PAT bearer.
//
// dpDirect is the PAT-accepting group (auth.WithPAT applied) that
// also has the per-PAT rate limiter attached. Each PAT-bearing
// route is additionally guarded by auth.RequireScope so a token
// minted with scopes={"read"} can't issue mutations.
func registerDatapackRoutes(g, dpDirect *echo.Group, reg *serviceRegistry, maxArtifactSizeBytes int64) {
	dpH := handlers.NewDatapackHandler(reg.dpSvc, reg.langfuseSvc, reg.memoriesSvc, reg.kbSvc)
	observeH := handlers.NewObserveHandler(reg.langfuseSvc)
	agentH := handlers.NewAgentHandler(reg.agentSvc)

	dpDirect.POST("/datapacks", dpH.Create, auth.RequireScope(auth.ScopeWrite))
	dpDirect.GET("/datapacks", dpH.List, auth.RequireScope(auth.ScopeRead))
	g.POST("/observe/session/bootstrap", observeH.BootstrapSession)

	// AMP (Postgres databases) inside a datapack — only when the feature is
	// enabled. Reads are allowed for anyone with datapack access (owner or
	// shared); writes (spin up database / branch / compute) are owner-only.
	if reg.ampSvc != nil {
		ampH := handlers.NewAMPHandler(reg.ampSvc)
		// AMP controller registration is managed by meko_admin (writes the
		// meko_system.amp_registration row directly); api_server only reads it.
		dpAccess := auth.RequireDatapackAccess(reg.dpRepo)
		dpOwner := auth.RequireDatapackOwnership(reg.dpRepo)
		g.POST("/datapacks/:datapack_id/clusters", ampH.CreateCluster, dpOwner)
		g.GET("/datapacks/:datapack_id/clusters", ampH.ListClusters, dpAccess)
		g.GET("/datapacks/:datapack_id/clusters/:cluster_id/branches", ampH.ListBranches, dpAccess)
		g.POST("/datapacks/:datapack_id/clusters/:cluster_id/branches", ampH.CreateBranch, dpOwner)
		g.GET("/datapacks/:datapack_id/clusters/:cluster_id/branches/:branch_id/compute", ampH.ListCompute, dpAccess)
		g.POST("/datapacks/:datapack_id/clusters/:cluster_id/branches/:branch_id/compute", ampH.CreateCompute, dpOwner)
	}

	g.POST("/agents", agentH.CreateUser)
	g.GET("/agents", agentH.ListUser)
	g.DELETE("/agents/:agent_id", agentH.DeleteUser)

	memoriesH := handlers.NewMemoriesHandler(reg.memoriesSvc, reg.langfuseSvc, reg.dpRepo)
	dpDirect.GET("/memories", memoriesH.ListGET, auth.RequireScope(auth.ScopeRead))
	dpDirect.GET("/memories/count", memoriesH.CountGET, auth.RequireScope(auth.ScopeRead))
	// Deletion is gated inside the handler (not via Echo middleware) because
	// the datapack id arrives as a query param, not a datapack path segment —
	// auth.RequireDatapackAccess expects the latter. The handler runs two
	// checks of its own: (1) caller is currently a member of the datapack
	// (owner or accepted share) and (2) the row's meko_user_id matches the
	// caller. Together these let any member delete their own rows while
	// blocking revoked members and cross-member deletes.
	// Disabled until the private-memory delete flow has end-to-end testing.
	// g.DELETE("/memories/:memory_id", memoriesH.DeleteByID)

	patH := handlers.NewMCPPATHandler(reg.mcpPATSvc)
	g.POST("/me/meko-tokens", patH.Create)
	g.GET("/me/meko-tokens", patH.List)
	g.DELETE("/me/meko-tokens/:id", patH.Revoke)

	// Token savings (contextual_prominence_search): self-service account
	// rollup on the JWT-only group, same as /me/meko-tokens above.
	tokenSavingsH := handlers.NewTokenSavingsHandler(reg.tokenSavingsSvc)
	g.GET("/me/token-savings", tokenSavingsH.GetForAccount)

	promoteMemoriesH := handlers.NewPromoteMemoriesHandler(reg.promoteMemoriesSvc, reg.dpRepo)
	// On dpDirect (not the JWT-only group) so BOTH the UI (Cognito JWT) and the
	// meko-mcp-server (PAT, mko_tkn_) can reach them — the JWT-only group rejects
	// PATs, which is why memory_promote over a PAT returned 401. The handlers
	// self-authorize the datapack (Promote requires owner/maintainer,
	// KeepAsMemory requires access) off the account WithPAT resolves, so a write
	// scope is the only added guard; JWT bearers carry pat_scopes=["*"], leaving
	// the UI unchanged. Matches the read sibling /promoted-memories below.
	dpDirect.POST("/promote-memories", promoteMemoriesH.Promote, auth.RequireScope(auth.ScopeWrite))
	dpDirect.POST("/keep-as-memory", promoteMemoriesH.KeepAsMemory, auth.RequireScope(auth.ScopeWrite))
	dpDirect.GET("/promoted-memories", promoteMemoriesH.ListGET, auth.RequireScope(auth.ScopeRead))

	feedbackH := handlers.NewFeedbackHandler(reg.feedbackRepo)
	g.POST("/feedback", feedbackH.Create)
	g.GET("/feedback", feedbackH.ListGET)

	shareH := handlers.NewSharingHandler(reg.sharingSvc)
	g.GET("/datapacks/incoming-shares", shareH.IncomingShares)
	g.POST("/datapacks/invite-action", shareH.InviteAction)

	// Describe allows both owners and shared users. Reachable via PAT
	// (read scope) so MCP clients can resolve a datapack by UUID.
	dpDirect.GET("/datapacks/:datapack_id", dpH.Describe,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Connect / MCP details: any member with datapack access (owner or
	// accepted share with viewer/contributor/maintainer grant).
	g.GET("/datapacks/:datapack_id/provision-status", handlers.DatapackProvisionStatusHandler(reg.dpRepo),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Langfuse-backed conversations: read-only; same access as Describe /
	// provision-status (owner or shared member with agent_access).
	// On dpDirect so PAT bearers (MCP server) are accepted.
	dpDirect.GET("/datapacks/:datapack_id/conversations", observeH.ConversationList,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	dpDirect.GET("/datapacks/:datapack_id/conversations/count", observeH.ConversationCount,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	dpDirect.GET("/datapacks/:datapack_id/conversations/:conversation_id", observeH.ConversationDetail,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	dpDirect.GET(
		"/datapacks/:datapack_id/conversations/:conversation_id/messages/:message_id/trace",
		observeH.MessageTrace,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	dpDirect.GET(
		"/datapacks/:datapack_id/conversations/:conversation_id/messages/:message_id/decision-trace",
		observeH.MessageDecisionTrace,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Token savings (contextual_prominence_search): read-only, same access
	// as conversations above (owner or shared member). On dpDirect so PAT
	// bearers (eval harnesses, MCP-adjacent tooling) are accepted.
	dpDirect.GET("/datapacks/:datapack_id/token-savings", tokenSavingsH.GetForDatapack,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Pending outgoing invites: owner or maintainer only.
	g.GET("/datapacks/:datapack_id/outgoing-shares", shareH.OutgoingShares,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	// Accepted members (roster): any role with datapack access.
	g.GET("/datapacks/:datapack_id/member-list", shareH.MemberList,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Invite / resend / revoke: owner or maintainer (not under RequireDatapackOwnership).
	// Middleware order is required: RequireDatapackAccess loads dp into context;
	// RequireDatapackOwnerOrMaintainerGrant only reads that context (see its doc comment).
	g.POST("/datapacks/:datapack_id/share", shareH.Invite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	g.POST("/datapacks/:datapack_id/invite/:invite_id/resend", shareH.ResendInvite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	g.DELETE("/datapacks/:datapack_id/invite/:invite_id", shareH.RevokeInvite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)

	named := g.Group("/datapacks/:datapack_id",
		auth.RequireDatapackOwnership(reg.dpRepo),
	)
	// dpNamedDirect mirrors `named` but lives on the PAT-accepting
	// group with per-route scope guards. Splitting the named subgroup
	// in two keeps the agent link/unlink routes JWT-only (admin-style
	// operations) while letting MCP clients hit the four datapack
	// owner mutations with a PAT.
	dpNamedDirect := dpDirect.Group("/datapacks/:datapack_id",
		auth.RequireDatapackOwnership(reg.dpRepo),
	)

	dpNamedDirect.PATCH("", dpH.Update, auth.RequireScope(auth.ScopeWrite))
	dpNamedDirect.DELETE("", dpH.Delete, auth.RequireScope(auth.ScopeWrite))
	dpNamedDirect.GET("/status", dpH.Status, auth.RequireScope(auth.ScopeRead))
	dpNamedDirect.POST("/langfuse/project-keys",
		dpH.GenerateLangfuseProjectKeys, auth.RequireScope(auth.ScopeWrite))

	named.POST("/agents", agentH.Link)
	named.DELETE("/agents/:agent_id", agentH.Unlink)

	kbH := handlers.NewKnowledgeBaseHandler(reg.kbSvc, reg.ragSvc, reg.s3Svc)

	// Read-only datapack routes: any member with datapack access (owner or accepted share),
	// not the ownership-only group below.
	g.GET("/datapacks/:datapack_id/agents", agentH.ListForDatapack,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:datapack_id/knowledge-bases/counts", kbH.Counts,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:datapack_id/knowledge-bases/documents", kbH.Documents,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: header tile aggregates + auto-generated
	// description/files-summary strings. Same grant gate as /documents
	// — read-only, anyone with access to the datapack can see it.
	g.GET("/datapacks/:datapack_id/knowledge-bases/summary", kbH.Summary,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: merged file-upload + memory-promotion feed.
	g.GET("/datapacks/:datapack_id/knowledge-bases/activity-log", kbH.ActivityLog,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: presigned S3 GET URL for a single
	// document. Read-only (downloads are available to anyone with
	// datapack access, not just owner/maintainer), so we hang it off
	// `RequireDatapackAccess` rather than the owner-or-maintainer
	// chain used by uploads.
	g.POST("/datapacks/:datapack_id/knowledge-bases/download-url", kbH.RequestDownloadURL,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Per-file DELETE: any datapack member can hit this, but the
	// handler enforces a tighter rule: owner / maintainer / uploader
	// of THIS file are allowed; everyone else gets 403 from inside
	// the handler. That's why we deliberately use
	// RequireDatapackAccess (not the owner-or-maintainer middleware)
	// — viewers and contributors who uploaded a file must be able
	// to delete their own contribution.
	g.DELETE("/datapacks/:datapack_id/knowledge-bases/documents/:document_id",
		kbH.DeleteDocument,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Knowledge base CRUD / upload: datapack owner or maintainer only (not viewer/contributor).
	// TODO(external-kb): POST /knowledge-bases is reserved for a future
	// "register an external KB URL" flow. The canonical per-datapack KB
	// row is created/refreshed via POST /knowledge-bases/create.
	kbOwnerOrMaintainer := []echo.MiddlewareFunc{
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	}
	g.POST("/datapacks/:datapack_id/knowledge-bases", kbH.Add, kbOwnerOrMaintainer...)
	g.GET("/datapacks/:datapack_id/knowledge-bases", kbH.List,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.DELETE("/datapacks/:datapack_id/knowledge-bases/:rag_source_id", kbH.Delete, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:datapack_id/knowledge-bases/upload-url", kbH.RequestUploadURL, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:datapack_id/knowledge-bases/upload-complete", kbH.ConfirmUpload, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:datapack_id/knowledge-bases/create", kbH.CreateKB, kbOwnerOrMaintainer...)
	g.GET("/datapacks/:datapack_id/knowledge-bases/status", kbH.KBStatus, kbOwnerOrMaintainer...)
	// Files PUT to S3 via a presigned URL but never registered with dist_rag
	// (user closed the dialog before clicking Create/Add-to-KB). Owner/maintainer
	// only — same gate as the upload/create path that produces these orphans.
	g.GET("/datapacks/:datapack_id/knowledge-bases/pending-uploads", kbH.PendingUploads, kbOwnerOrMaintainer...)
	g.DELETE("/datapacks/:datapack_id/knowledge-bases/pending-uploads", kbH.DeletePendingUpload, kbOwnerOrMaintainer...)

	// Update a member's grant: owner or maintainer only (same gate as invite/resend/revoke).
	// Uses RequireDatapackAccess so shared maintainers can act; dp.UserID in the handler
	// always refers to the datapack row owner, not the caller.
	g.PATCH("/datapacks/:datapack_id/share/:share_user_id", shareH.UpdateGrant,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)

	artifactH := handlers.NewArtifactHandler(reg.artifactSvc, maxArtifactSizeBytes)
	// Artifact upload: any member with write access (owner or maintainer).
	// Artifact get: any member with datapack access (owner, maintainer, contributor, viewer).
	dpDirect.POST("/datapacks/:datapack_id/artifacts", artifactH.Put,
		auth.RequireScope(auth.ScopeWrite),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	dpDirect.GET("/datapacks/:datapack_id/artifacts/:content_hash", artifactH.Get,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	slog.Info("datapack routes registered")
}
