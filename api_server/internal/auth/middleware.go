package auth

import (
	"context"
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
)

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
			tokenStr, err := extractBearerToken(c.Request())
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

// extractBearerToken pulls the token from the "Authorization: Bearer <token>"
// header. Returns an error if the header is missing or malformed.
func extractBearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
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

// Sentinel errors for authorization header extraction.
var (
	ErrMissingAuthHeader   = newAuthErr("missing authorization header")
	ErrMalformedAuthHeader = newAuthErr("malformed authorization header")
)

type authErr struct {
	msg string
}

func (e *authErr) Error() string { return e.msg }

func newAuthErr(msg string) *authErr {
	return &authErr{msg: msg}
}
