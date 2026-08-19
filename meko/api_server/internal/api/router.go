package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	echoSwagger "github.com/swaggo/echo-swagger"

	"meko/api_server/docs" // registers the swagger spec with swag
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
	adminFeedbackSvc   *adminsvc.AdminFeedbackService
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

	// envSvc is nil unless control-plane mode is enabled (--control-plane-mode);
	// the YCP fleet + agent routes are registered only when it is non-nil.
	envSvc *service.EnvironmentService

	// Marketplace: nil until AWS_MARKETPLACE_PRODUCT_CODE is set.
	// Route registration guards on marketplaceSvc.Enabled().
	marketplaceRepo       *repository.MarketplaceRepo
	marketplaceSvc        *service.MarketplaceService
	marketplaceWelcomeURL string
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
	adminFeedbackSvc := adminsvc.NewAdminFeedbackService(adminRepo)

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
		// Datapack deletion cleans up the datapack's AMP clusters + pool mapping.
		dpSvc.SetAMPService(ampSvc)
		slog.Info("amp routes enabled (controller endpoint resolved from amp_registration)")
	} else {
		slog.Warn("amp routes disabled (set --show-pg-amp to enable)")
	}

	// YCP fleet registry (BYOC control plane). Enabled only in control-plane mode.
	var envSvc *service.EnvironmentService
	if cfg.ControlPlaneMode {
		envSvc = service.NewEnvironmentService(repository.NewEnvironmentRepo(cfg.SystemDB), cfg.BootstrapDefaults)
		slog.Info("control-plane mode enabled (YCP fleet + agent routes)")
	}

	artifactRepo := repository.NewArtifactRepo(cfg.DB)
	artifactSvc := service.NewArtifactService(artifactRepo, s3Svc)

	tokenSavingsRepo := repository.NewTokenSavingsRepo(cfg.SystemDB)
	tokenSavingsSvc := service.NewTokenSavingsService(tokenSavingsRepo, dpRepo)

	// AWS Marketplace: nil until AWS_MARKETPLACE_PRODUCT_CODE is set on
	// the pod. NewMarketplaceService returns (nil, nil) in that state
	// so handlers 503 cleanly and nothing panics.
	marketplaceRepo := repository.NewMarketplaceRepo(cfg.SystemDB)
	marketplaceSvc, mErr := service.NewMarketplaceService(
		context.Background(),
		service.MarketplaceConfig{
			ProductCode: cfg.AWSMarketplaceProductCode,
			Region:      cfg.AWSMarketplaceRegion,
			RoleARN:     cfg.AWSMarketplaceRoleARN,
		},
		marketplaceRepo,
		billingRepo,
	)
	if mErr != nil {
		slog.Error("marketplace: init failed; routes will 503", "error", mErr)
	}

	slog.Info("service registry initialized")
	return &serviceRegistry{
		accountRepo:           accountRepo,
		ybaRepo:               ybaRepo,
		ybaSvc:                ybaSvc,
		dpRepo:                dpRepo,
		dpSvc:                 dpSvc,
		kbRepo:                kbRepo,
		freeTierRepo:          freeTierRepo,
		accountUsageSvc:       accountUsageSvc,
		langfuseSvc:           langfuseSvc,
		agentSvc:              agentSvc,
		kbSvc:                 kbSvc,
		ragSvc:                ragSvc,
		memoriesSvc:           memoriesSvc,
		promoteMemoriesSvc:    promoteMemoriesSvc,
		s3Svc:                 s3Svc,
		sharingRepo:           sharingRepo,
		sharingSvc:            sharingSvc,
		feedbackRepo:          feedbackRepo,
		mcpPATRepo:            mcpPATRepo,
		mcpPATAuditRepo:       mcpPATAuditRepo,
		mcpPATSvc:             mcpPATSvc,
		billingRepo:           billingRepo,
		billingSvc:            billingSvc,
		adminMetricsSvc:       adminMetricsSvc,
		adminDataPlaneSvc:     adminDataPlaneSvc,
		adminFeedbackSvc:      adminFeedbackSvc,
		artifactRepo:          artifactRepo,
		artifactSvc:           artifactSvc,
		tokenSavingsRepo:      tokenSavingsRepo,
		tokenSavingsSvc:       tokenSavingsSvc,
		ampSvc:                ampSvc,
		ampRegistrationRepo:   ampRegistrationRepo,
		envSvc:                envSvc,
		marketplaceRepo:       marketplaceRepo,
		marketplaceSvc:        marketplaceSvc,
		marketplaceWelcomeURL: cfg.AWSMarketplaceWelcomeURL,
	}
}

// NewRouter builds an Echo instance with all API routes registered.
// When Cognito config is provided, auth routes are registered and
// datapack routes require a valid JWT. When Cognito config is empty
// (e.g. in tests), all routes are public.
//
// Every route hangs off `root`, which is cfg.BasePath (empty by default, so
// the mount table is unchanged for every deployment that serves at the domain
// root). With --base-path=/ycp the whole surface -- console, /api/v1, swagger,
// the OAuth2 callback -- moves under /ycp, which is what lets the control plane
// sit behind https://<host>/ycp/ on a shared domain.
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

	// Refuse to start on a base path that isn't one. Booting anyway would serve
	// 404 everywhere while still passing the health probe below, which reads as
	// a healthy deployment.
	basePath, err := normalizeBasePath(cfg.BasePath)
	if err != nil {
		return nil, nil, err
	}
	if basePath != "" {
		slog.Info("serving every route under a base path", "base_path", basePath)
	}
	root := e.Group(basePath)

	// The swagger spec is generated with basePath /api/v1 baked in; move it with
	// the routes so "Try it out" targets paths that actually exist.
	docs.SwaggerInfo.BasePath = basePath + "/api/v1"
	root.GET("/swagger/*", echoSwagger.WrapHandler)

	reg := buildServiceRegistry(cfg)

	var successCfg *handlers.SuccessConfig

	// One limiter instance shared by the group and the root health alias below,
	// so the alias cannot hand out a second per-IP budget.
	mgmtRateLimiter := ManagementRateLimiter()
	v1 := root.Group("/api/v1", mgmtRateLimiter)

	v1.GET("/health", handlers.HealthHandler(cfg.SystemDB, cfg.DB))
	// Health also stays at the domain root when a base path is set. Container
	// probes and load-balancer checks hit the pod directly, bypassing the proxy
	// that supplies the prefix, so they would 404 on the prefixed path -- and a
	// chart that predates --base-path shouldn't fail its readiness gate the
	// moment the flag is turned on. It carries the same rate limiter as the
	// prefixed copy: the handler pings both databases, so an unthrottled route
	// to it is an unauthenticated way to saturate the connection pools.
	if basePath != "" {
		e.GET("/api/v1/health", handlers.HealthHandler(cfg.SystemDB, cfg.DB), mgmtRateLimiter)
	}

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
	adminH := handlers.NewAdminHandler(reg.adminMetricsSvc, reg.adminDataPlaneSvc, reg.adminFeedbackSvc)
	adminGroup.GET("/users", adminH.Users)
	adminGroup.GET("/metrics", adminH.Metrics)
	adminGroup.GET("/token-savings", adminH.Executive)
	adminGroup.GET("/dataplane", adminH.DataPlane)
	adminGroup.PATCH("/users/:id/tier", adminH.UpdateUserTier)
	adminGroup.GET("/users/:id/tier-audit", adminH.TierAudit)
	adminGroup.GET("/feedback", adminH.Feedback)

	// AMP controller registration, managed by meko_admin through api_server so
	// the write path shares api_server's system-DB connection.
	ampRegH := handlers.NewAMPRegistrationHandler(reg.ampRegistrationRepo)
	adminGroup.GET("/amp-registration", ampRegH.Get)
	adminGroup.POST("/amp-registration", ampRegH.Register)
	adminGroup.PATCH("/amp-registration", ampRegH.Patch)
	adminGroup.DELETE("/amp-registration", ampRegH.Delete)

	// YCP fleet registry + agent API (control-plane mode). Console/fleet routes
	// are gated by the admin shared secret; agent routes authenticate with the
	// enrollment token (enroll) and env-scoped status token (status/desired-state).
	if reg.envSvc != nil {
		envH := handlers.NewEnvironmentHandler(reg.envSvc)
		// The secret the console page carries, if this deployment opted in. Empty
		// otherwise, and the console asks for it in-page as before.
		consoleSecret := ""
		if cfg.ConsoleEmbedAdminSecret {
			consoleSecret = cfg.AdminAPIKey
		}
		// One middleware for the whole group rather than a check per handler, so a
		// route added later cannot forget it.
		fleetAuth := []echo.MiddlewareFunc{
			auth.RequireAdminSecret(cfg.AdminAPIKey),
			// Always on: a malformed account is a 400 whether or not one is required.
			auth.ValidateAccountID(),
		}
		if cfg.RequireAccountHeader {
			fleetAuth = append(fleetAuth, auth.RequireAccountID())
		}
		fleet := v1.Group("", fleetAuth...)
		fleet.GET("/environments", envH.List)
		fleet.POST("/environments", envH.Register)
		fleet.GET("/environments/:id", envH.Get)
		fleet.PATCH("/environments/:id", envH.Action)
		fleet.DELETE("/environments/:id", envH.Deregister)
		fleet.GET("/environments/:id/desired", envH.GetDesired)
		fleet.POST("/environments/:id/desired", envH.SetDesired)
		fleet.GET("/entitlements", envH.Entitlements)
		agentGroup := v1.Group("/agent")
		agentGroup.POST("/enroll", envH.AgentEnroll)
		agentGroup.GET("/desired-state", envH.AgentDesiredState)
		agentGroup.POST("/status", envH.AgentStatus)
		agentGroup.POST("/bootstrap-progress", envH.AgentBootstrapProgress)

		// The fleet console, served from this binary so the control plane is one
		// thing to deploy. Registered on the base-path root, not the /api/v1
		// group, and only in control-plane mode — a data-plane api-server keeps
		// returning 404 at / as before.
		//
		// Under a base path both /ycp/ and /ycp serve the console: a person
		// types the latter, and Echo treats the two as distinct routes.
		root.GET("/", handlers.ConsoleHandler(consoleSecret))
		if basePath != "" {
			e.GET(basePath, handlers.ConsoleHandler(consoleSecret))
		}
	}

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

	root.GET("/success", handlers.SuccessHandler(successCfg))

	registerDatapackRoutes(datapackGroup, dpDirect, reg, cfg.MaxArtifactSizeBytes, cfg.EnableConversationSearch)
	registerBillingRoutes(v1, datapackGroup, reg)
	registerMarketplaceRoutes(v1, datapackGroup, reg)

	return e, reg, nil
}

// registerMarketplaceRoutes wires the AWS Marketplace fulfillment +
// SNS endpoints. Skipped entirely when MarketplaceService is nil (no
// ProductCode configured), so a Meko instance that hasn't finished
// its AWS listing yet just doesn't expose the routes.
//
// The fulfillment endpoint attaches to `authed` because the buyer
// must be signed in to Meko for us to know which user to bind the
// entitlement to. SNS attaches to `v1` directly because AWS SNS calls
// are unauthenticated — payload authenticity is proven by the SNS
// message signature (TODO in the handler).
func registerMarketplaceRoutes(v1 *echo.Group, authed *echo.Group, reg *serviceRegistry) {
	if reg.marketplaceSvc == nil || !reg.marketplaceSvc.Enabled() {
		slog.Warn("marketplace routes disabled (AWS_MARKETPLACE_PRODUCT_CODE not set)")
		return
	}
	h := handlers.NewMarketplaceHandler(
		reg.marketplaceSvc, reg.marketplaceRepo, reg.billingRepo, reg.marketplaceWelcomeURL,
	)
	// Register is PUBLIC — AWS Marketplace form-POSTs here from
	// aws.amazon.com and the buyer may not yet have a Meko session.
	// The handler stashes the token and bounces to /signup (new
	// buyer) or /marketplace/complete (existing user).
	v1.POST("/marketplace/register", h.Register)
	// Complete requires a Meko session. Called by the browser after
	// signup / when an already-signed-in user gets bounced from
	// Register; reads the stashed token and finalizes the entitlement.
	authed.GET("/marketplace/complete", h.Complete)
	v1.POST("/marketplace/sns", h.SNS)
	slog.Info("marketplace routes registered")
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
func registerDatapackRoutes(g, dpDirect *echo.Group, reg *serviceRegistry, maxArtifactSizeBytes int64, enableConversationSearch bool) {
	dpH := handlers.NewDatapackHandler(reg.dpSvc, reg.langfuseSvc, reg.memoriesSvc, reg.kbSvc)
	observeH := handlers.NewObserveHandler(reg.langfuseSvc, enableConversationSearch)
	agentH := handlers.NewAgentHandler(reg.agentSvc)

	dpDirect.POST("/datapacks", dpH.Create, auth.RequireScope(auth.ScopeWrite))
	dpDirect.GET("/datapacks", dpH.List, auth.RequireScope(auth.ScopeRead))
	g.POST("/observe/session/bootstrap", observeH.BootstrapSession)

	// AMP (Postgres databases) inside a datapack — only when the feature is
	// enabled. On dpDirect (NOT the JWT-only group) so BOTH the UI (Cognito JWT)
	// and the meko-mcp-server (PAT, mko_tkn_) can reach these — the JWT-only
	// group rejects PATs. Reads need datapack access (owner or shared) + read
	// scope; writes/deletes (spin up / tear down database / branch / compute)
	// are owner-only + write scope.
	if reg.ampSvc != nil {
		ampH := handlers.NewAMPHandler(reg.ampSvc)
		// AMP controller registration is managed by meko_admin (writes the
		// meko_system.amp_registration row directly); api_server only reads it.
		dpAccess := auth.RequireDatapackAccess(reg.dpRepo)
		dpOwner := auth.RequireDatapackOwnership(reg.dpRepo)
		read := auth.RequireScope(auth.ScopeRead)
		write := auth.RequireScope(auth.ScopeWrite)
		dpDirect.POST("/datapacks/:datapack_id/amp/clusters", ampH.CreateCluster, write, dpOwner)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters", ampH.ListClusters, read, dpAccess)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters/:cluster_id", ampH.GetCluster, read, dpAccess)
		dpDirect.DELETE("/datapacks/:datapack_id/amp/clusters/:cluster_id", ampH.DeleteCluster, write, dpOwner)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches", ampH.ListBranches, read, dpAccess)
		dpDirect.POST("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches", ampH.CreateBranch, write, dpOwner)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id", ampH.GetBranch, read, dpAccess)
		dpDirect.DELETE("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id", ampH.DeleteBranch, write, dpOwner)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id/compute", ampH.ListCompute, read, dpAccess)
		dpDirect.POST("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id/compute", ampH.CreateCompute, write, dpOwner)
		dpDirect.GET("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id/compute/:compute_id", ampH.GetCompute, read, dpAccess)
		dpDirect.POST("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id/compute/:compute_id/resize", ampH.ResizeCompute, write, dpOwner)
		dpDirect.DELETE("/datapacks/:datapack_id/amp/clusters/:cluster_id/branches/:branch_id/compute/:compute_id", ampH.DeleteCompute, write, dpOwner)
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
	// Direct file upload: single-call alternative to the presigned flow for
	// programmatic/agentic callers. On dpDirect so a PAT (not just a JWT) works.
	// There is no BodyLimit middleware on this route: the handler caps the raw
	// body itself so the limit also covers chunked uploads, which declare no
	// Content-Length for middleware to inspect.
	dpDirect.POST("/datapacks/:datapack_id/knowledge-bases/upload", kbH.UploadFile,
		auth.RequireScope(auth.ScopeWrite),
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	g.POST("/datapacks/:datapack_id/knowledge-bases/create", kbH.CreateKB, kbOwnerOrMaintainer...)
	g.GET("/datapacks/:datapack_id/knowledge-bases/status", kbH.KBStatus, kbOwnerOrMaintainer...)
	// Single-document status: PAT-reachable so the direct-upload flow can poll a
	// single file (via the status_url in its 202 response).
	dpDirect.GET("/datapacks/:datapack_id/knowledge-bases/document-status", kbH.KBDocumentStatus,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
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
	// Paginated artifact manifest for the datapack detail Artifacts tile + the
	// full Artifacts page. Any datapack grant can read this (matches single-GET
	// semantics).
	dpDirect.GET("/datapacks/:datapack_id/artifacts", artifactH.List,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// Short-lived presigned S3 GET for the browser Download button. Falls
	// through to the base64 GET path for db-inline artifacts (response has
	// no URL, storage tier = "db").
	dpDirect.POST("/datapacks/:datapack_id/artifacts/:content_hash/download-url", artifactH.RequestDownloadURL,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	slog.Info("datapack routes registered")
}
