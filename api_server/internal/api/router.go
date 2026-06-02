package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/labstack/echo/v4"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "meko/api_server/docs" // registers swagger spec with swag
	"meko/api_server/internal/auth"
	"meko/api_server/internal/email"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// serviceRegistry holds every repository and service the router needs,
// built once and shared across all route-registration helpers.
type serviceRegistry struct {
	accountRepo        *repository.AccountRepo
	ybaRepo            *repository.YBARepo
	ybaSvc             *service.YBAService
	dpRepo             *repository.DatapackRepo
	dpSvc              *service.DatapackService
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
	ragSvc := service.NewRagService(cfg.DB, service.DatapackIndexSchema, service.DatapackIndexName)

	memoriesRepo := repository.NewMemoriesRepo(cfg.DB, cfg.SystemDB, cfg.OpenAIAPIKey, cfg.OpenAILLMModel, cfg.OpenAIEmbeddingModel, cfg.MemoryListSkipVectorSort)
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

	dpSvc := service.NewDatapackService(dpRepo, ybaSvc, accountRepo, langfuseSvc)

	mcpPATRepo := repository.NewMCPPATRepo(cfg.SystemDB)
	mcpPATAuditRepo := repository.NewMCPPATAuditRepo(cfg.SystemDB)
	mcpPATSvc := service.NewMCPPATService(mcpPATRepo, mcpPATAuditRepo)

	slog.Info("service registry initialized")
	return &serviceRegistry{
		accountRepo:        accountRepo,
		ybaRepo:            ybaRepo,
		ybaSvc:             ybaSvc,
		dpRepo:             dpRepo,
		dpSvc:              dpSvc,
		langfuseSvc:        langfuseSvc,
		agentSvc:           agentSvc,
		kbSvc:              kbSvc,
		ragSvc:             ragSvc,
		memoriesSvc:        memoriesSvc,
		promoteMemoriesSvc: promoteMemoriesSvc,
		s3Svc:              s3Svc,
		sharingRepo:        sharingRepo,
		sharingSvc:         sharingSvc,
		feedbackRepo:       feedbackRepo,
		mcpPATRepo:         mcpPATRepo,
		mcpPATAuditRepo:    mcpPATAuditRepo,
		mcpPATSvc:          mcpPATSvc,
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
		datapackGroup = v1.Group("", auth.DevAuth(reg.accountRepo))
		dpDirect = datapackGroup
	}

	e.GET("/success", handlers.SuccessHandler(successCfg))

	registerDatapackRoutes(datapackGroup, dpDirect, reg)

	return e, reg, nil
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
	authH := handlers.NewAuthHandler(authSvc)

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
	// Change password is gated by requireAuth so the access-token cookie
	// is guaranteed present when the handler runs; Cognito then verifies
	// the previous-password against the user resolved from that token.
	authGroup.POST("/change-password", authH.ChangePassword, requireAuth)
	authGroup.GET("/me", authH.Me, requireAuth)
	authGroup.PATCH("/me", authH.UpdateOnboardingStatus, requireAuth)

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
func registerDatapackRoutes(g, dpDirect *echo.Group, reg *serviceRegistry) {
	dpH := handlers.NewDatapackHandler(reg.dpSvc, reg.langfuseSvc, reg.memoriesSvc, reg.kbSvc)
	observeH := handlers.NewObserveHandler(reg.langfuseSvc)
	agentH := handlers.NewAgentHandler(reg.agentSvc)

	dpDirect.POST("/datapacks", dpH.Create, auth.RequireScope(auth.ScopeWrite))
	dpDirect.GET("/datapacks", dpH.List, auth.RequireScope(auth.ScopeRead))
	g.POST("/observe/session/bootstrap", observeH.BootstrapSession)

	g.POST("/agents", agentH.CreateUser)
	g.GET("/agents", agentH.ListUser)
	g.DELETE("/agents/:agent_id", agentH.DeleteUser)

	memoriesH := handlers.NewMemoriesHandler(reg.memoriesSvc, reg.langfuseSvc, reg.dpRepo)
	g.GET("/memories", memoriesH.ListGET)
	g.GET("/memories/count", memoriesH.CountGET)
	// Deletion is gated inside the handler (not via Echo middleware) because
	// the datapack id arrives as a query param, not a `:name` path segment —
	// auth.RequireDatapackAccess expects the latter. The handler runs two
	// checks of its own: (1) caller is currently a member of the datapack
	// (owner or accepted share) and (2) the row's meko_user_id matches the
	// caller. Together these let any member delete their own rows while
	// blocking revoked members and cross-member deletes.
	g.DELETE("/memories/:memory_id", memoriesH.DeleteByID)

	patH := handlers.NewMCPPATHandler(reg.mcpPATSvc)
	g.POST("/me/meko-tokens", patH.Create)
	g.GET("/me/meko-tokens", patH.List)
	g.DELETE("/me/meko-tokens/:id", patH.Revoke)

	promoteMemoriesH := handlers.NewPromoteMemoriesHandler(reg.promoteMemoriesSvc, reg.dpRepo)
	g.POST("/promote-memories", promoteMemoriesH.Promote)
	g.POST("/keep-as-memory", promoteMemoriesH.KeepAsMemory)
	g.GET("/promoted-memories", promoteMemoriesH.ListGET)

	feedbackH := handlers.NewFeedbackHandler(reg.feedbackRepo)
	g.POST("/feedback", feedbackH.Create)
	g.GET("/feedback", feedbackH.ListGET)

	shareH := handlers.NewSharingHandler(reg.sharingSvc)
	g.GET("/datapacks/incoming-shares", shareH.IncomingShares)
	g.POST("/datapacks/invite-action", shareH.InviteAction)

	// Describe allows both owners and shared users. Reachable via PAT
	// (read scope) so MCP clients can resolve a datapack by name.
	dpDirect.GET("/datapacks/:name", dpH.Describe,
		auth.RequireScope(auth.ScopeRead),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Connect / MCP details: any member with datapack access (owner or
	// accepted share with viewer/contributor/maintainer grant).
	g.GET("/datapacks/:name/provision-status", handlers.DatapackProvisionStatusHandler(reg.dpRepo),
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Langfuse-backed conversations: read-only; same access as Describe /
	// provision-status (owner or shared member with agent_access).
	g.GET("/datapacks/:name/conversations", observeH.ConversationList,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:name/conversations/count", observeH.ConversationCount,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:name/conversations/:conversation_id", observeH.ConversationDetail,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:name/conversations/:conversation_id/messages/:message_id/trace", observeH.MessageTrace,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Pending outgoing invites: owner or maintainer only.
	g.GET("/datapacks/:name/outgoing-shares", shareH.OutgoingShares,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	// Accepted members (roster): any role with datapack access.
	g.GET("/datapacks/:name/member-list", shareH.MemberList,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Invite / resend / revoke: owner or maintainer (not under RequireDatapackOwnership).
	// Middleware order is required: RequireDatapackAccess loads dp into context;
	// RequireDatapackOwnerOrMaintainerGrant only reads that context (see its doc comment).
	g.POST("/datapacks/:name/share", shareH.Invite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	g.POST("/datapacks/:name/invite/:invite_id/resend", shareH.ResendInvite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)
	g.DELETE("/datapacks/:name/invite/:invite_id", shareH.RevokeInvite,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)

	named := g.Group("/datapacks/:name",
		auth.RequireDatapackOwnership(reg.dpRepo),
	)
	// dpNamedDirect mirrors `named` but lives on the PAT-accepting
	// group with per-route scope guards. Splitting the named subgroup
	// in two keeps the agent link/unlink routes JWT-only (admin-style
	// operations) while letting MCP clients hit the four datapack
	// owner mutations with a PAT.
	dpNamedDirect := dpDirect.Group("/datapacks/:name",
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
	g.GET("/datapacks/:name/agents", agentH.ListForDatapack,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:name/knowledge-bases/counts", kbH.Counts,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.GET("/datapacks/:name/knowledge-bases/documents", kbH.Documents,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: header tile aggregates + auto-generated
	// description/files-summary strings. Same grant gate as /documents
	// — read-only, anyone with access to the datapack can see it.
	g.GET("/datapacks/:name/knowledge-bases/summary", kbH.Summary,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: merged file-upload + memory-promotion feed.
	g.GET("/datapacks/:name/knowledge-bases/activity-log", kbH.ActivityLog,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	// SharedKnowledge page: presigned S3 GET URL for a single
	// document. Read-only (downloads are available to anyone with
	// datapack access, not just owner/maintainer), so we hang it off
	// `RequireDatapackAccess` rather than the owner-or-maintainer
	// chain used by uploads.
	g.POST("/datapacks/:name/knowledge-bases/download-url", kbH.RequestDownloadURL,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	// Per-file DELETE: any datapack member can hit this, but the
	// handler enforces a tighter rule: owner / maintainer / uploader
	// of THIS file are allowed; everyone else gets 403 from inside
	// the handler. That's why we deliberately use
	// RequireDatapackAccess (not the owner-or-maintainer middleware)
	// — viewers and contributors who uploaded a file must be able
	// to delete their own contribution.
	g.DELETE("/datapacks/:name/knowledge-bases/documents/:document_id",
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
	g.POST("/datapacks/:name/knowledge-bases", kbH.Add, kbOwnerOrMaintainer...)
	g.GET("/datapacks/:name/knowledge-bases", kbH.List,
		auth.RequireDatapackAccess(reg.dpRepo),
	)
	g.DELETE("/datapacks/:name/knowledge-bases/:rag_source_id", kbH.Delete, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:name/knowledge-bases/upload-url", kbH.RequestUploadURL, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:name/knowledge-bases/upload-complete", kbH.ConfirmUpload, kbOwnerOrMaintainer...)
	g.POST("/datapacks/:name/knowledge-bases/create", kbH.CreateKB, kbOwnerOrMaintainer...)
	g.GET("/datapacks/:name/knowledge-bases/status", kbH.KBStatus, kbOwnerOrMaintainer...)

	// Update a member's grant: owner or maintainer only (same gate as invite/resend/revoke).
	// Uses RequireDatapackAccess so shared maintainers can act; dp.UserID in the handler
	// always refers to the datapack row owner, not the caller.
	g.PATCH("/datapacks/:name/share/:share_user_id", shareH.UpdateGrant,
		auth.RequireDatapackAccess(reg.dpRepo),
		auth.RequireDatapackOwnerOrMaintainerGrant(),
	)

	slog.Info("datapack routes registered")
}
