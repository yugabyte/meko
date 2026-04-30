package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

// Context keys used to store authenticated account data on the request.
const (
	ContextKeyAccount    = "account"
	ContextKeyCognitoSub = "cognito_sub"
	ContextKeyDatapack   = "datapack"
)

// GetAccount extracts the authenticated account from the Echo context.
// Returns ErrNoAccount if the account is missing (i.e. RequireAuth did not run).
func GetAccount(c echo.Context) (*models.Account, error) {
	slog.Info("getting account", "context", c.Get(ContextKeyAccount))
	account, ok := c.Get(ContextKeyAccount).(*models.Account)
	if !ok || account == nil {
		return nil, ErrNoAccount
	}
	return account, nil
}

// GetDatapack extracts the resolved datapack from the Echo context.
// Returns an error if RequireDatapackOwnership middleware did not run.
func GetDatapack(c echo.Context) (*models.Datapack, error) {
	datapack, ok := c.Get(ContextKeyDatapack).(*models.Datapack)
	if !ok || datapack == nil {
		return nil, fmt.Errorf("datapack not found in context")
	}
	return datapack, nil
}

// AccountLookup abstracts account resolution for the auth middleware.
// EnsureAccount must return an existing account or auto-create one for
// the given Cognito sub (just-in-time provisioning for users who signed
// up via the Cognito Hosted UI).
type AccountLookup interface {
	EnsureAccount(ctx context.Context, sub, email string) (*models.Account, error)
}

// RequireAuth returns Echo middleware that validates the JWT access token
// in the Authorization header, looks up the corresponding account, and
// attaches it to the request context.
//
// Protected routes behind this middleware can retrieve the account with:
//
//	account, ok := c.Get(auth.ContextKeyAccount).(*models.Account)
func RequireAuth(
	validator *JWTValidator,
	accounts AccountLookup,
) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			tokenStr, err := ExtractBearerToken(c.Request())
			if err != nil {
				slog.Debug("auth missing or malformed header",
					"path", c.Path(),
					"error", err,
				)
				return authError(c, "missing or malformed authorization header")
			}

			claims, err := validator.ValidateAccessToken(tokenStr)
			if err != nil {
				slog.Warn("auth token validation failed",
					"path", c.Path(),
					"error", err,
				)
				return authError(c, "invalid or expired token")
			}

			account, err := accounts.EnsureAccount(
				c.Request().Context(), claims.Sub, claims.Username,
			)
			if err != nil {
				slog.Error("auth account resolution failed",
					"cognito_sub", claims.Sub,
					"error", err,
				)
				return authError(c, "account lookup failed")
			}

			c.Set(ContextKeyAccount, account)
			c.Set(ContextKeyCognitoSub, claims.Sub)

			slog.Debug("auth succeeded",
				"account_id", account.ID,
				"cognito_sub", claims.Sub,
				"path", c.Path(),
			)

			return next(c)
		}
	}
}

// DatapackOwnershipLookup abstracts datapack lookup for the ownership
// middleware. Typically implemented by repository.DatapackRepo.
//
// The lookup hits meko_system.datapacks via the
// UNIQUE (user_id, datapack_name) constraint so the URL-friendly :name
// parameter remains a stable identifier per user.
type DatapackOwnershipLookup interface {
	GetByName(userID, name string) (*models.Datapack, error)
}

// RequireDatapackOwnership returns Echo middleware that verifies the
// authenticated user owns the datapack identified by the :name route
// parameter. Must run after RequireAuth.
//
// On success the resolved *models.Datapack is stored on the context
// under ContextKeyDatapack so downstream handlers can reuse it without
// a second lookup. Cross-user (shared) access is intentionally not
// considered here; sharing-aware authorization will live in a separate
// middleware once those endpoints land.
func RequireDatapackOwnership(datapacks DatapackOwnershipLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			account, err := GetAccount(c)
			if err != nil {
				return authError(c, "missing account context")
			}

			name := c.Param("name")
			if name == "" {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "datapack name is required",
					},
				})
			}

			dp, err := datapacks.GetByName(account.ID, name)
			if err != nil {
				slog.Error("ownership lookup failed",
					"datapack", name, "user_id", account.ID, "error", err)
				return c.JSON(http.StatusInternalServerError, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "INTERNAL_ERROR",
						"message": "failed to verify datapack ownership",
					},
				})
			}
			if dp == nil {
				return c.JSON(http.StatusNotFound, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "NOT_FOUND",
						"message": fmt.Sprintf("datapack %q not found", name),
					},
				})
			}

			c.Set(ContextKeyDatapack, dp)
			return next(c)
		}
	}
}

// DatapackAccessLookup abstracts datapack lookup for the access
// middleware: ownership first, then shared access via agent_access.
type DatapackAccessLookup interface {
	GetByName(userID, name string) (*models.Datapack, error)
	GetByNameForSharedAccess(userID, name string) (*models.Datapack, error)
}

// ContextKeyAccessLevel is set by RequireDatapackAccess to "owner" or
// "shared" so handlers can branch on access level.
const ContextKeyAccessLevel = "access_level"

// RequireDatapackAccess returns Echo middleware that verifies the
// authenticated user either owns the datapack or has shared access to
// it via agent_access. Must run after RequireAuth.
//
// On success the resolved *models.Datapack is stored under
// ContextKeyDatapack and the access level ("owner" or "shared") under
// ContextKeyAccessLevel.
func RequireDatapackAccess(datapacks DatapackAccessLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			account, err := GetAccount(c)
			if err != nil {
				return authError(c, "missing account context")
			}

			name := c.Param("name")
			if name == "" {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "datapack name is required",
					},
				})
			}

			dp, err := datapacks.GetByName(account.ID, name)
			if err != nil {
				slog.Error("access lookup failed",
					"datapack", name, "user_id", account.ID, "error", err)
				return c.JSON(http.StatusInternalServerError, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "INTERNAL_ERROR",
						"message": "failed to verify datapack access",
					},
				})
			}
			if dp != nil {
				c.Set(ContextKeyDatapack, dp)
				c.Set(ContextKeyAccessLevel, "owner")
				return next(c)
			}

			dp, err = datapacks.GetByNameForSharedAccess(account.ID, name)
			if err != nil {
				slog.Error("shared access lookup failed",
					"datapack", name, "user_id", account.ID, "error", err)
				return c.JSON(http.StatusInternalServerError, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "INTERNAL_ERROR",
						"message": "failed to verify datapack access",
					},
				})
			}
			if dp == nil {
				return c.JSON(http.StatusNotFound, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "NOT_FOUND",
						"message": fmt.Sprintf("datapack %q not found", name),
					},
				})
			}

			c.Set(ContextKeyDatapack, dp)
			c.Set(ContextKeyAccessLevel, "shared")
			return next(c)
		}
	}
}

// DevAuth returns middleware that injects a dev account into the request
// context, auto-creating one in the database on first use. This enables
// local testing without Cognito. Must only be used when auth is disabled.
func DevAuth(accounts AccountLookup) echo.MiddlewareFunc {
	const devSub = "dev-local"
	const devEmail = "dev@localhost"

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			account, err := accounts.EnsureAccount(
				c.Request().Context(), devSub, devEmail,
			)
			if err != nil {
				slog.Error("dev auth: failed to ensure dev account", "error", err)
				return authError(c, "dev account setup failed")
			}
			c.Set(ContextKeyAccount, account)
			c.Set(ContextKeyCognitoSub, devSub)
			slog.Debug("dev auth injected", "account_id", account.ID)
			return next(c)
		}
	}
}

// ExtractBearerToken pulls the token from the "Authorization: Bearer <token>"
// header. Returns an error if the header is missing or malformed.
func ExtractBearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		// Cookie fallback for browser-based auth flows.
		if c, err := r.Cookie("access_token"); err == nil {
			token := strings.TrimSpace(c.Value)
			if token != "" {
				return token, nil
			}
		}
		return "", ErrMissingAuthHeader
	}

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrMalformedAuthHeader
	}

	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", ErrMissingAuthHeader
	}

	return token, nil
}

// authError sends a 401 JSON response using the standard API envelope.
func authError(c echo.Context, message string) error {
	return c.JSON(http.StatusUnauthorized, map[string]any{
		"data": nil,
		"error": map[string]any{
			"code":    "UNAUTHORIZED",
			"message": message,
		},
	})
}

// Sentinel errors.
var (
	ErrMissingAuthHeader   = newAuthErr("missing authorization header")
	ErrMalformedAuthHeader = newAuthErr("malformed authorization header")
	ErrNoAccount           = newAuthErr("no account in request context")
)

type authErr struct {
	msg string
}

func (e *authErr) Error() string { return e.msg }

func newAuthErr(msg string) *authErr {
	return &authErr{msg: msg}
}
