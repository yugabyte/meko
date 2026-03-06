package handlers

import (
	"database/sql"
	"net/http"
)

// HealthHandler returns a handler that reports server and database health.
func HealthHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		dbStatus := "ok"
		if db == nil {
			dbStatus = "unavailable"
		} else if err := db.Ping(); err != nil {
			dbStatus = "unavailable"
		}

		WriteJSON(w, http.StatusOK, map[string]string{
			"status":   "ok",
			"database": dbStatus,
		})
	}
}
