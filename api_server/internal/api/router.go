package api

import (
	"database/sql"

	"github.com/labstack/echo/v4"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "meko/api_server/docs" // registers swagger spec with swag
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// NewRouter builds an Echo instance with all API routes registered.
func NewRouter(db *sql.DB) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = handlers.CustomHTTPErrorHandler
	RegisterMiddleware(e)

	e.GET("/swagger/*", echoSwagger.WrapHandler)

	v1 := e.Group("/api/v1", ManagementRateLimiter())

	v1.GET("/health", handlers.HealthHandler(db))

	ybaRepo := repository.NewYBARepo(db)
	ybaSvc := service.NewYBAService(ybaRepo)

	dpRepo := repository.NewDatapackRepo(db)
	dpSvc := service.NewDatapackService(dpRepo, ybaSvc)
	dpH := handlers.NewDatapackHandler(dpSvc)

	v1.POST("/datapacks", dpH.Create)
	v1.GET("/datapacks", dpH.List)
	v1.GET("/datapacks/:name", dpH.Describe)
	v1.PATCH("/datapacks/:name", dpH.Update)
	v1.DELETE("/datapacks/:name", dpH.Delete)
	v1.GET("/datapacks/:name/status", dpH.Status)

	agentRepo := repository.NewAgentRepo(db)
	agentSvc := service.NewAgentService(agentRepo)
	agentH := handlers.NewAgentHandler(agentSvc)

	v1.POST("/datapacks/:name/agents", agentH.Create)
	v1.GET("/datapacks/:name/agents", agentH.List)
	v1.DELETE("/datapacks/:name/agents/:agent", agentH.Delete)

	kbRepo := repository.NewKnowledgeBaseRepo(db)
	kbSvc := service.NewKnowledgeBaseService(kbRepo)
	kbH := handlers.NewKnowledgeBaseHandler(kbSvc)

	v1.POST("/datapacks/:name/knowledge-bases", kbH.Add)
	v1.GET("/datapacks/:name/knowledge-bases", kbH.List)
	v1.DELETE("/datapacks/:name/knowledge-bases", kbH.Delete)

	return e
}
