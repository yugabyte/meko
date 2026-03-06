package api

import (
	"database/sql"
	"net/http"

	"meko/api_server/internal/handlers"
)

// NewRouter builds the top-level HTTP mux with all API routes registered.
func NewRouter(db *sql.DB) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", handlers.HealthHandler(db))

	var handler http.Handler = mux
	handler = LoggingMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	handler = CORSMiddleware(handler)

	return handler
}
