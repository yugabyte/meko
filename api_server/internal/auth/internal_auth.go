package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// InternalSecretHeader is the request header expected to carry the
// shared secret protecting machine-to-machine endpoints (e.g. the
// MCP server -> api_server PAT validation call). The constant value
// is a header NAME, not a secret -- gosec's G101 pattern matches on
// the word "secret" in the identifier and triggers a false positive
// here.
const InternalSecretHeader = "X-Meko-Internal-Secret" //nolint:gosec // header name, not a credential

// RequireInternalSecret returns Echo middleware that gates a route
// group on a static shared secret.
//
// When secret == "":
//
//	The endpoint is treated as disabled and every request returns
//	503. This is the default in dev / test environments where no
//	internal-services secret has been provisioned, and prevents
//	accidental exposure of an open validator.
//
// When secret != "":
//
//	The X-Meko-Internal-Secret header is required and must match
//	via constant-time comparison. Mismatch returns 401 with the
//	standard error envelope. The middleware never logs the header
//	value -- only path and remote address -- so a wrong secret in a
//	curl test does not bleed into production logs.
//
// This middleware is intentionally NOT a substitute for the
// JWT-based RequireAuth on user-facing endpoints. It exists for
// trusted server-to-server traffic only and should be wired on a
// dedicated /internal/* route group.
func RequireInternalSecret(secret string) echo.MiddlewareFunc {
	expected := []byte(secret)
	enabled := len(expected) > 0
	if !enabled {
		slog.Warn("internal-secret middleware initialized with empty secret; endpoints will return 503")
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !enabled {
				return c.JSON(http.StatusServiceUnavailable, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "SERVICE_UNAVAILABLE",
						"message": "internal endpoint disabled (no shared secret configured)",
					},
				})
			}

			provided := []byte(c.Request().Header.Get(InternalSecretHeader))
			if subtle.ConstantTimeCompare(provided, expected) != 1 {
				slog.Warn("internal endpoint auth failed",
					"path", c.Path(),
					"remote_addr", c.Request().RemoteAddr,
				)
				return c.JSON(http.StatusUnauthorized, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "UNAUTHORIZED",
						"message": "missing or invalid internal secret",
					},
				})
			}

			return next(c)
		}
	}
}
