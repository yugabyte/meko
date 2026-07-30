package handlers

import (
	"embed"
	"net/http"

	"github.com/labstack/echo/v4"
)

// The fleet console, compiled in so control-plane mode has no static-asset
// dependency at runtime. It started as a copy of ycp/web/console.html, which
// could not be shared because api_server and ycp are separate Go modules and
// go:embed cannot reach across module boundaries; that copy is gone with the
// reference control plane, so this is now the only one.
//
//go:embed web/console.html
var consoleFS embed.FS

// ConsoleHandler serves the fleet console at /. Registered only in
// control-plane mode, so a data-plane api-server still 404s at the root.
func ConsoleHandler() echo.HandlerFunc {
	return func(c echo.Context) error {
		b, err := consoleFS.ReadFile("web/console.html")
		if err != nil {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "console asset missing")
		}
		return c.HTMLBlob(http.StatusOK, b)
	}
}
