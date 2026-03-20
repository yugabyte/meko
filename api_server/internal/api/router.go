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

	var successCfg *handlers.SuccessConfig

	v1 := e.Group("/api/v1", ManagementRateLimiter())

	v1.GET("/health", handlers.HealthHandler(cfg.DB))

	authEnabled := cfg.CognitoRegion != "" &&
		cfg.CognitoPoolID != "" &&
		cfg.CognitoClientID != ""

	datapackGroup := v1.Group("")

	if authEnabled {
		var authDeps *authRouteDeps
		authDeps, err := registerAuthRoutes(
			cfg, v1, &datapackGroup,
		)
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
		slog.Warn("cognito config not set, auth disabled")
	}

	e.GET("/success", handlers.SuccessHandler(successCfg))

	registerDatapackRoutes(cfg, datapackGroup)

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
) (*authRouteDeps, error) {
	accountRepo := repository.NewAccountRepo(cfg.DB)

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

	authSvc := service.NewAuthService(cognitoClient, accountRepo)
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
		auth.RequireAuth(jwtValidator, accountRepo),
	)

	slog.Info("auth middleware enabled on protected routes")
	return &authRouteDeps{
		cognitoClient: cognitoClient,
		authService:   authSvc,
	}, nil
}

// registerDatapackRoutes wires all datapack, agent, and knowledge-base
// routes onto the given group.
func registerDatapackRoutes(
	cfg ServerConfig,
	g *echo.Group,
) {
	ybaRepo := repository.NewYBARepo(cfg.DB)
	ybaSvc := service.NewYBAService(ybaRepo)

	dpRepo := repository.NewDatapackRepo(cfg.DB)
	dpSvc := service.NewDatapackService(dpRepo, ybaSvc)
	dpH := handlers.NewDatapackHandler(dpSvc)

	g.POST("/datapacks", dpH.Create)
	g.GET("/datapacks", dpH.List)

	named := g.Group("/datapacks/:name",
		auth.RequireDatapackOwnership(dpRepo),
	)

	named.GET("", dpH.Describe)
	named.PATCH("", dpH.Update)
	named.DELETE("", dpH.Delete)
	named.GET("/status", dpH.Status)

	agentRepo := repository.NewAgentRepo(cfg.DB)
	agentSvc := service.NewAgentService(agentRepo)
	agentH := handlers.NewAgentHandler(agentSvc)

	named.POST("/agents", agentH.Create)
	named.GET("/agents", agentH.List)
	named.DELETE("/agents/:agent", agentH.Delete)

	kbRepo := repository.NewKnowledgeBaseRepo(cfg.DB)
	kbSvc := service.NewKnowledgeBaseService(kbRepo)
	kbH := handlers.NewKnowledgeBaseHandler(kbSvc)

	named.POST("/knowledge-bases", kbH.Add)
	named.GET("/knowledge-bases", kbH.List)
	named.DELETE("/knowledge-bases", kbH.Delete)
}
