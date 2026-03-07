package api

import (
	"database/sql"
	"net/http"

	"meko/api_server/internal/handlers"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// NewRouter builds the top-level HTTP mux with all API routes registered.
func NewRouter(db *sql.DB) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", handlers.HealthHandler(db))

	dpRepo := repository.NewDatapackRepo(db)
	dpSvc := service.NewDatapackService(dpRepo)
	dpHandler := handlers.NewDatapackHandler(dpSvc)

	mux.HandleFunc("POST /api/v1/datapacks", dpHandler.Create)
	mux.HandleFunc("GET /api/v1/datapacks", dpHandler.List)
	mux.HandleFunc(
		"GET /api/v1/datapacks/{name}", dpHandler.Describe,
	)
	mux.HandleFunc(
		"PATCH /api/v1/datapacks/{name}", dpHandler.Update,
	)
	mux.HandleFunc(
		"DELETE /api/v1/datapacks/{name}", dpHandler.Delete,
	)
	mux.HandleFunc(
		"GET /api/v1/datapacks/{name}/status", dpHandler.Status,
	)

	var handler http.Handler = mux
	handler = LoggingMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	handler = CORSMiddleware(handler)

	return handler
}
