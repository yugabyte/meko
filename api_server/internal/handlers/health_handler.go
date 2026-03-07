package handlers

import (
	"database/sql"
	"net/http"

	"github.com/labstack/echo/v4"
)

// HealthHandler returns an Echo handler that reports server and DB health.
func HealthHandler(db *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		dbStatus := "ok"
		if db == nil {
			dbStatus = "unavailable"
		} else if err := db.Ping(); err != nil {
			dbStatus = "unavailable"
		}

		return ok(c, http.StatusOK, map[string]string{
			"status":   "ok",
			"database": dbStatus,
		})
	}
}
