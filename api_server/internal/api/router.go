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
	"meko/api_server/internal/k8s"
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
	tierRepo    *repository.TierConfigRepo
	agentSvc    *service.AgentService
	kbSvc       *service.KnowledgeBaseService
}

// buildServiceRegistry initialises all repositories and services in
// dependency order so that every component shares a single instance.
func buildServiceRegistry(cfg ServerConfig) *serviceRegistry {
	slog.Info("initializing service registry")

	accountRepo := repository.NewAccountRepo(cfg.DB)
	ybaRepo := repository.NewYBARepo(cfg.DB)
	dpRepo := repository.NewDatapackRepo(cfg.DB)
	tierRepo := repository.NewTierConfigRepo(cfg.DB)

	ybaSvc := service.NewYBAService(ybaRepo)
	dpSvc := service.NewDatapackService(dpRepo, ybaSvc, accountRepo)

	agentRepo := repository.NewAgentRepo(cfg.DB)
	agentSvc := service.NewAgentService(agentRepo)

	kbRepo := repository.NewKnowledgeBaseRepo(cfg.DB)
	kbSvc := service.NewKnowledgeBaseService(kbRepo)

	langfuseSvc := service.NewLangfuseService(service.LangfuseConfig{
		DB:          cfg.DB,
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
		tierRepo:    tierRepo,
		agentSvc:    agentSvc,
		kbSvc:       kbSvc,
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

	authSvc := service.NewAuthService(cognitoClient, reg.accountRepo, authOpts...)
	authH := handlers.NewAuthHandler(authSvc)

	authGroup := v1.Group("/auth")
	authGroup.POST("/signup", authH.Signup)
	authGroup.POST("/confirm-signup", authH.ConfirmSignup)
	authGroup.POST("/login", authH.Login)
	authGroup.POST("/logout", authH.Logout)

	jwtValidator := auth.NewJWTValidator(
		cfg.CognitoRegion,
		cfg.CognitoPoolID,
		cfg.CognitoClientID,
	)
	*datapackGroup = v1.Group("",
		auth.RequireAuth(jwtValidator, reg.accountRepo),
	)
	slog.Info("auth routes registered")
	return &authRouteDeps{
		cognitoClient: cognitoClient,
		authService:   authSvc,
	}, nil
}

// buildProvisioner attempts to create a Provisioner backed by the full
// ProvisioningService (K8s CRD + datapack). When the K8s client is
// unavailable it falls back to a datapack-only provisioner so that
// default datapacks are still created on signup.
func buildProvisioner(cfg ServerConfig, reg *serviceRegistry) service.Provisioner {
	k8sClient, err := k8s.NewClient()
	if err != nil {
		slog.Warn("k8s client unavailable, falling back to datapack-only provisioner",
			"error", err,
		)
		return &datapackOnlyProvisioner{dpSvc: reg.dpSvc}
	}

	provSvc := service.NewProvisioningService(
		reg.dpSvc,
		k8sClient,
		reg.tierRepo,
		reg.accountRepo,
		reg.langfuseSvc,
		service.ProvisioningConfig{CRDNamespace: cfg.CRDNamespace},
	)
	slog.Info("full provisioner enabled", "crd_namespace", cfg.CRDNamespace)
	return provSvc
}

// datapackOnlyProvisioner satisfies service.Provisioner by creating a
// default datapack without applying any K8s CRDs.
type datapackOnlyProvisioner struct {
	dpSvc *service.DatapackService
}

func (p *datapackOnlyProvisioner) ProvisionNewAccount(
	ctx context.Context,
	accountID string,
) {
	if p == nil || p.dpSvc == nil {
		return
	}
	_, err := p.dpSvc.Create(service.CreateRequest{
		Name:      service.DefaultDatapackName,
		AccountID: accountID,
	})
	if err != nil {
		slog.Error("failed to provision default datapack",
			"account_id", accountID,
			"error", err,
		)
		return
	}
	slog.Info("default datapack provisioned (datapack-only mode)",
		"account_id", accountID,
	)
}

// registerDatapackRoutes wires all datapack, observe, agent, and
// knowledge-base routes onto the given group.
func registerDatapackRoutes(g *echo.Group, reg *serviceRegistry) {
	dpH := handlers.NewDatapackHandler(reg.dpSvc, reg.langfuseSvc)
	observeH := handlers.NewObserveHandler(reg.langfuseSvc)

	g.GET("/internal/provision-status", handlers.ProvisionStatusHandler(reg.dpRepo))

	g.POST("/datapacks", dpH.Create)
	g.GET("/datapacks", dpH.List)
	g.POST("/observe/session/bootstrap", observeH.BootstrapSession)

	named := g.Group("/datapacks/:name",
		auth.RequireDatapackOwnership(reg.dpRepo),
	)

	named.GET("", dpH.Describe)
	named.PATCH("", dpH.Update)
	named.DELETE("", dpH.Delete)
	named.GET("/status", dpH.Status)
	named.POST("/langfuse/project-keys", dpH.GenerateLangfuseProjectKeys)

	agentH := handlers.NewAgentHandler(reg.agentSvc)
	named.POST("/agents", agentH.Create)
	named.GET("/agents", agentH.List)
	named.DELETE("/agents/:agent", agentH.Delete)

	kbH := handlers.NewKnowledgeBaseHandler(reg.kbSvc)
	named.POST("/knowledge-bases", kbH.Add)
	named.GET("/knowledge-bases", kbH.List)
	named.DELETE("/knowledge-bases", kbH.Delete)

	slog.Info("datapack routes registered")
}
