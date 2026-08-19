package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// InternalSecretHeader is the request header expected to carry the
// shared secret protecting machine-to-machine endpoints (e.g. the
// MCP server -> api_server PAT validation call). The constant value
// is a header NAME, not a secret -- gosec's G101 pattern matches on
// the word "secret" in the identifier and triggers a false positive
// here.
const InternalSecretHeader = "X-Meko-Internal-Secret" //nolint:gosec // header name, not a credential

// AdminSecretHeader is the request header carrying the shared secret
// that protects the /admin/* metrics endpoints consumed by meko_admin.
// Like InternalSecretHeader this is a header NAME, not a credential.
const AdminSecretHeader = "X-Meko-Admin-Secret" //nolint:gosec // header name, not a credential

// AccountIDHeader is the request header naming the account a fleet call acts
// for. YBM stamps it from the account it has already authenticated, so it is
// trusted to the same degree as the admin secret that gates the route.
//
// Optional unless the control plane runs --require-account-header: without it a
// caller that sends no account stays unscoped and sees the whole fleet, which is
// what the console, the ycp e2e and an operator's curl rely on.
const AccountIDHeader = "X-Meko-Account-ID"

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
	return requireSecret(InternalSecretHeader, secret)
}

// RequireAdminSecret gates the /admin/* metrics endpoints on a static
// shared secret carried in the X-Meko-Admin-Secret header, using the
// same disabled-when-empty / constant-time-compare semantics as
// RequireInternalSecret. Intended for the trusted meko_admin console.
func RequireAdminSecret(secret string) echo.MiddlewareFunc {
	return requireSecret(AdminSecretHeader, secret)
}

// RequireAccountID returns middleware rejecting any request that does not name an
// account in AccountIDHeader, for a control plane where every fleet caller is
// expected to be an account-aware proxy such as YBM.
//
// It is wired only when --require-account-header is set, because it takes the
// unscoped fleet-admin path away: the built-in console, the ycp e2e and an
// operator's curl all authenticate with the admin secret alone and have no
// account to send, so they get 400 from a control plane running with it on.
//
// It does not validate the value. What the account is allowed to see follows from
// the id itself, and the admin secret this sits behind is already the thing that
// says the caller may speak for one.
func RequireAccountID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if strings.TrimSpace(c.Request().Header.Get(AccountIDHeader)) == "" {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": AccountIDHeader + " required",
					},
				})
			}
			return next(c)
		}
	}
}

// requireSecret builds shared-secret middleware for a given header:
// 503 when secret is empty, 401 on mismatch (constant-time compare),
// and never logs the header value.
func requireSecret(header, secret string) echo.MiddlewareFunc {
	expected := []byte(secret)
	enabled := len(expected) > 0
	if !enabled {
		slog.Warn("shared-secret middleware initialized with empty secret; endpoints will return 503",
			"header", header)
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !enabled {
				return c.JSON(http.StatusServiceUnavailable, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "SERVICE_UNAVAILABLE",
						"message": "endpoint disabled (no shared secret configured)",
					},
				})
			}

			provided := []byte(c.Request().Header.Get(header))
			if subtle.ConstantTimeCompare(provided, expected) != 1 {
				slog.Warn("shared-secret endpoint auth failed",
					"path", c.Path(),
					"remote_addr", c.Request().RemoteAddr,
				)
				return c.JSON(http.StatusUnauthorized, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "UNAUTHORIZED",
						"message": "missing or invalid secret",
					},
				})
			}

			return next(c)
		}
	}
}
