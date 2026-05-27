package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// DbStatus is the reachability status reported by HealthHandler for a
// single database connection.
type DbStatus string

const (
	DbStatusOK          DbStatus = "ok"
	DbStatusUnavailable DbStatus = "unavailable"
)

// HealthHandler returns an Echo handler that reports server and DB
// health. The `database` field stays for backward compatibility and
// reports `ok` only when both DBs are reachable; the per-DB breakdown
// is exposed under `system_database` / `data_database`.
//
//	@Summary	Health check
//	@Tags		health
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Router		/health [get]
func HealthHandler(systemDB, dataDB *sql.DB) echo.HandlerFunc {
	return func(c echo.Context) error {
		systemStatus := pingStatus(systemDB)
		dataStatus := pingStatus(dataDB)
		overall := DbStatusOK
		if systemStatus != DbStatusOK || dataStatus != DbStatusOK {
			overall = DbStatusUnavailable
		}

		return ok(c, http.StatusOK, map[string]string{
			"status":          "ok",
			"database":        string(overall),
			"system_database": string(systemStatus),
			"data_database":   string(dataStatus),
		})
	}
}

func pingStatus(db *sql.DB) DbStatus {
	if db == nil {
		return DbStatusUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return DbStatusUnavailable
	}
	return DbStatusOK
}
