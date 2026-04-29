package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/labstack/echo/v4"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "meko/api_server/docs" // registers swagger spec with swag
	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// serviceRegistry holds every repository and service the router needs,
// built once and shared across all route-registration helpers.
type serviceRegistry struct {
	accountRepo *repository.AccountRepo
	ybaRepo     *repository.YBARepo
	ybaSvc      *service.YBAService
	dpRepo      *repository.DatapackRepo
	dpSvc       *service.DatapackService
	langfuseSvc *service.LangfuseService
	agentSvc    *service.AgentService
	kbSvc       *service.KnowledgeBaseService
	ragSvc      *service.RagService
	memoriesSvc        *service.MemoriesService
	promoteMemoriesSvc *service.PromoteMemoriesService
	s3Svc              *service.S3Service
	sharingRepo *repository.SharingRepo
	sharingSvc  *service.SharingService
}

// buildServiceRegistry initialises all repositories and services in
// dependency order so that every component shares a single instance.
func buildServiceRegistry(cfg ServerConfig) *serviceRegistry {
	slog.Info("initializing service registry")

	accountRepo := repository.NewAccountRepo(cfg.DB)
	ybaRepo := repository.NewYBARepo(cfg.DB)
	dpRepo := repository.NewDatapackRepo(cfg.DB)

	ybaSvc := service.NewYBAService(ybaRepo)
	dpSvc := service.NewDatapackService(dpRepo, ybaSvc, accountRepo)

	agentRepo := repository.NewAgentRepo(cfg.DB)
	agentSvc := service.NewAgentService(agentRepo)

	kbRepo := repository.NewKnowledgeBaseRepo(cfg.DB)
	kbSvc := service.NewKnowledgeBaseService(kbRepo, cfg.S3Bucket)
	ragSvc := service.NewRagService(cfg.DB)

	memoriesRepo := repository.NewMemoriesRepo(cfg.DB, cfg.OpenAIAPIKey, cfg.OpenAILLMModel, cfg.OpenAIEmbeddingModel)
	memoriesSvc := service.NewMemoriesService(memoriesRepo)

	promoteMemoriesRepo := repository.NewPromoteMemoriesRepo(cfg.DB)
	promoteMemoriesSvc := service.NewPromoteMemoriesService(promoteMemoriesRepo)

	sharingRepo := repository.NewSharingRepo(cfg.DB)
	sharingSvc := service.NewSharingService(sharingRepo, nil)
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
		DB:          cfg.DB,
		LangfuseDB:  cfg.LangfuseDB,
		Host:        cfg.LangfuseHost,
		AdminAPIKey: cfg.LangfuseAdminAPIKey,
	})
	if !langfuseSvc.Enabled() {
		slog.Warn("langfuse integration disabled; set host + admin api key to enable")
	}

	slog.Info("service registry initialized")
	return &serviceRegistry{
		accountRepo: accountRepo,
		ybaRepo:     ybaRepo,
		ybaSvc:      ybaSvc,
		dpRepo:      dpRepo,
		dpSvc:       dpSvc,
		langfuseSvc: langfuseSvc,
		agentSvc:    agentSvc,
		kbSvc:       kbSvc,
		ragSvc:      ragSvc,
		memoriesSvc:        memoriesSvc,
		promoteMemoriesSvc: promoteMemoriesSvc,
		s3Svc:              s3Svc,
		sharingRepo: sharingRepo,
		sharingSvc:  sharingSvc,
	}
}

// NewRouter builds an Echo instance with all API routes registered.
// When Cognito config is provided, auth routes are registered and
// datapack routes require a valid JWT. When Cognito config is empty
// (e.g. in tests), all routes are public.
func NewRouter(cfg ServerConfig) (*echo.Echo, error) {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = handlers.CustomHTTPErrorHandler
	RegisterMiddleware(e)

	e.GET("/swagger/*", echoSwagger.WrapHandler)

	reg := buildServiceRegistry(cfg)

	var successCfg *handlers.SuccessConfig

	v1 := e.Group("/api/v1", ManagementRateLimiter())

	v1.GET("/health", handlers.HealthHandler(cfg.DB))

	memoriesHTTPHandler := handlers.NewMemoriesHandler(reg.memoriesSvc)
	v1.GET("/memories", memoriesHTTPHandler.ListGET)
	v1.GET("/memories/count", memoriesHTTPHandler.CountGET)

	promoteMemoriesHandler := handlers.NewPromoteMemoriesHandler(reg.promoteMemoriesSvc)
	v1.POST("/promote-memories", promoteMemoriesHandler.Promote)

	authEnabled := cfg.CognitoRegion != "" &&
		cfg.CognitoPoolID != "" &&
		cfg.CognitoClientID != ""

	datapackGroup := v1.Group("")

	if authEnabled {
		authDeps, err := registerAuthRoutes(cfg, v1, &datapackGroup, reg)
		if err != nil {
			return nil, err
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
	}

	e.GET("/success", handlers.SuccessHandler(successCfg))

	registerDatapackRoutes(datapackGroup, reg)

	return e, nil
}

// authRouteDeps holds the initialized auth objects needed by the
// success handler.
type authRouteDeps struct {
	cognitoClient *auth.CognitoClient
	authService   *service.AuthService
}

// registerAuthRoutes wires the public /auth endpoints and configures
// the datapackGroup to require JWT authentication.
func registerAuthRoutes(
	cfg ServerConfig,
	v1 *echo.Group,
	datapackGroup **echo.Group,
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
	requireAuth := auth.RequireAuth(jwtValidator, reg.accountRepo)

	authGroup := v1.Group("/auth")
	authGroup.POST("/signup", authH.Signup)
	authGroup.POST("/confirm-signup", authH.ConfirmSignup)
	authGroup.POST("/login", authH.Login)
	authGroup.POST("/logout", authH.Logout)
	authGroup.GET("/me", authH.Me, requireAuth)
	authGroup.PATCH("/me", authH.UpdateOnboardingStatus, requireAuth)

	*datapackGroup = v1.Group("", requireAuth)
	slog.Info("auth routes registered")
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
// knowledge-base routes onto the given group.
func registerDatapackRoutes(g *echo.Group, reg *serviceRegistry) {
	dpH := handlers.NewDatapackHandler(reg.dpSvc, reg.langfuseSvc, reg.memoriesSvc, reg.kbSvc)
	observeH := handlers.NewObserveHandler(reg.langfuseSvc)
	agentH := handlers.NewAgentHandler(reg.agentSvc)

	g.POST("/datapacks", dpH.Create)
	g.GET("/datapacks", dpH.List)
	g.POST("/observe/session/bootstrap", observeH.BootstrapSession)

	g.POST("/agents", agentH.CreateUser)
	g.GET("/agents", agentH.ListUser)
	g.DELETE("/agents/:agent_id", agentH.DeleteUser)

	shareH := handlers.NewSharingHandler(reg.sharingSvc)
	g.GET("/datapacks/incoming-shares", shareH.IncomingShares)
	g.POST("/datapacks/invite-action", shareH.InviteAction)

	// Describe allows both owners and shared users.
	g.GET("/datapacks/:name", dpH.Describe,
		auth.RequireDatapackAccess(reg.dpRepo),
	)

	named := g.Group("/datapacks/:name",
		auth.RequireDatapackOwnership(reg.dpRepo),
	)

	named.PATCH("", dpH.Update)
	named.DELETE("", dpH.Delete)
	named.GET("/status", dpH.Status)
	named.POST("/langfuse/project-keys", dpH.GenerateLangfuseProjectKeys)

	// Per-datapack ysql credentials + the (account-level) MCP server
	// URL / API key. Shown in the datapack's Connect dialog.
	// Ownership is enforced by RequireDatapackOwnership on the named group.
	named.GET("/provision-status", handlers.DatapackProvisionStatusHandler(reg.dpRepo))

	// Read-only listing of Langfuse sessions (= chat conversations) for
	// the datapack. Paginated with limit/offset; requires the api server
	// to be started with --langfuse-db-host.
	named.GET("/conversations", observeH.ConversationList)
	named.GET("/conversations/count", observeH.ConversationCount)

	named.POST("/agents", agentH.Link)
	named.GET("/agents", agentH.ListForDatapack)
	named.DELETE("/agents/:agent_id", agentH.Unlink)

	kbH := handlers.NewKnowledgeBaseHandler(reg.kbSvc, reg.ragSvc, reg.s3Svc)
	// TODO(external-kb): POST /knowledge-bases is reserved for a future
	// "register an external KB URL" flow. The canonical per-datapack KB
	// row is created/refreshed via POST /knowledge-bases/create.
	named.POST("/knowledge-bases", kbH.Add)
	named.GET("/knowledge-bases", kbH.List)
	named.DELETE("/knowledge-bases/:rag_source_id", kbH.Delete)
	named.POST("/knowledge-bases/upload-url", kbH.RequestUploadURL)
	named.POST("/knowledge-bases/upload-complete", kbH.ConfirmUpload)
	named.POST("/knowledge-bases/create", kbH.CreateKB)
	named.GET("/knowledge-bases/status", kbH.KBStatus)

	named.GET("/knowledge-bases/counts", kbH.Counts)
	named.GET("/knowledge-bases/documents", kbH.Documents)

	named.POST("/share", shareH.Invite)
	named.GET("/outgoing-shares", shareH.OutgoingShares)
	named.POST("/invite/:invite_id/resend", shareH.ResendInvite)
	named.DELETE("/invite/:invite_id", shareH.RevokeInvite)
	named.PATCH("/share/:share_user_id", shareH.UpdateGrant)

	slog.Info("datapack routes registered")
}
