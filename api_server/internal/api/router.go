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

	v1 := e.Group("/api/v1", ManagementRateLimiter())

	v1.GET("/health", handlers.HealthHandler(cfg.DB))

	authEnabled := cfg.CognitoRegion != "" &&
		cfg.CognitoPoolID != "" &&
		cfg.CognitoClientID != ""

	datapackGroup := v1.Group("")

	if authEnabled {
		if err := registerAuthRoutes(
			cfg, v1, &datapackGroup,
		); err != nil {
			return nil, err
		}
	} else {
		slog.Warn("cognito config not set, auth disabled")
	}

	registerDatapackRoutes(cfg, datapackGroup)

	return e, nil
}

// registerAuthRoutes wires the public /auth endpoints and configures
// the datapackGroup to require JWT authentication.
func registerAuthRoutes(
	cfg ServerConfig,
	v1 *echo.Group,
	datapackGroup **echo.Group,
) error {
	accountRepo := repository.NewAccountRepo(cfg.DB)

	cognitoClient, err := auth.NewCognitoClient(
		context.Background(),
		cfg.CognitoRegion,
		cfg.CognitoPoolID,
		cfg.CognitoClientID,
		cfg.CognitoClientSecret,
	)
	if err != nil {
		return fmt.Errorf("initializing cognito client: %w", err)
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
	return nil
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
	g.GET("/datapacks/:name", dpH.Describe)
	g.PATCH("/datapacks/:name", dpH.Update)
	g.DELETE("/datapacks/:name", dpH.Delete)
	g.GET("/datapacks/:name/status", dpH.Status)

	agentRepo := repository.NewAgentRepo(cfg.DB)
	agentSvc := service.NewAgentService(agentRepo)
	agentH := handlers.NewAgentHandler(agentSvc)

	g.POST("/datapacks/:name/agents", agentH.Create)
	g.GET("/datapacks/:name/agents", agentH.List)
	g.DELETE("/datapacks/:name/agents/:agent", agentH.Delete)

	kbRepo := repository.NewKnowledgeBaseRepo(cfg.DB)
	kbSvc := service.NewKnowledgeBaseService(kbRepo)
	kbH := handlers.NewKnowledgeBaseHandler(kbSvc)

	g.POST("/datapacks/:name/knowledge-bases", kbH.Add)
	g.GET("/datapacks/:name/knowledge-bases", kbH.List)
	g.DELETE("/datapacks/:name/knowledge-bases", kbH.Delete)
}
