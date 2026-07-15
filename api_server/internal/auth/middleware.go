package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

// Context keys used to store authenticated account data on the request.
//
// ContextKeyAuthMethod tells downstream code (handlers, scope checks,
// access logger) which credential authenticated this request:
//   - AuthMethodCognito -- Cognito JWT bearer (or DevAuth in local mode).
//   - AuthMethodPAT     -- meko PAT bearer (mko_tkn_*).
//
// ContextKeyPATID and ContextKeyPATScopes are populated only on the
// PAT path. ContextKeyPATScopes is also set on the JWT path to
// {"*"} so RequireScope checks short-circuit cleanly.
const (
	ContextKeyAccount    = "account"
	ContextKeyCognitoSub = "cognito_sub"
	ContextKeyDatapack   = "datapack"
	ContextKeyAuthMethod = "auth_method"
	ContextKeyPATID      = "pat_id"
	ContextKeyPATScopes  = "pat_scopes"
)

// Auth method values stored at ContextKeyAuthMethod.
const (
	AuthMethodCognito = "cognito"
	AuthMethodPAT     = "pat"
)

// PAT scope vocabulary. Mirrored in the service package
// (service.ScopeAll/Read/Write) which owns mint-time validation;
// these duplicates exist here so middleware code (RequireScope,
// router wiring) can refer to them without pulling the service
// package, which would create an auth<->service import cycle.
const (
	ScopeAll   = "*"
	ScopeRead  = "read"
	ScopeWrite = "write"
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
// Returns an error if neither RequireDatapackOwnership nor
// RequireDatapackAccess has populated ContextKeyDatapack.
func GetDatapack(c echo.Context) (*models.Datapack, error) {
	datapack, ok := c.Get(ContextKeyDatapack).(*models.Datapack)
	if !ok || datapack == nil {
		return nil, fmt.Errorf("datapack not found in context")
	}
	return datapack, nil
}

// AccountLookup abstracts account resolution for the auth middleware.
//
//   - EnsureAccount drives the JWT path: returns an existing account or
//     auto-creates one for the given Cognito sub (JIT provisioning for
//     users who signed up via the Cognito Hosted UI).
//   - GetByID drives the PAT path: a PAT is bound to a user_id at
//     mint time, so we just look the row up by its PK. Returns
//     (nil, nil) when the account no longer exists (e.g. the user was
//     deleted but their PAT row was not yet purged).
type AccountLookup interface {
	EnsureAccount(ctx context.Context, sub, email string) (*models.Account, error)
	GetByID(ctx context.Context, id string) (*models.Account, error)
}

// PATValidateInput carries the per-request data needed to validate a
// PAT bearer. The auth package owns this struct so it never needs to
// import the service package (which would cycle, since service imports
// auth for HashPAT / GeneratePAT).
type PATValidateInput struct {
	Plaintext      string
	ActorIP        string
	ActorUserAgent string
}

// PATValidateResult is the auth-side projection of the validation
// verdict. Mirrors models.ValidatePATResponse but lives in this
// package so middleware can branch on it without an import cycle.
//
// On Valid=true: TokenID, UserID, and Scopes are populated; Reason is
// empty. On Valid=false: only Reason is populated.
type PATValidateResult struct {
	Valid   bool
	TokenID string
	UserID  string
	Scopes  []string
	Reason  string
}

// PATValidator is the narrow interface RequireAuth depends on when
// the WithPAT option is supplied. Concrete implementation lives in
// the service layer; router.go bridges via PATValidatorFunc.
type PATValidator interface {
	ValidatePAT(ctx context.Context, in PATValidateInput) (*PATValidateResult, error)
}

// PATValidatorFunc is the function-typed adapter that lets the
// router build a PATValidator from any service-side closure without
// declaring a separate type. Standard adapter pattern.
type PATValidatorFunc func(ctx context.Context, in PATValidateInput) (*PATValidateResult, error)

// ValidatePAT implements PATValidator.
func (f PATValidatorFunc) ValidatePAT(
	ctx context.Context,
	in PATValidateInput,
) (*PATValidateResult, error) {
	return f(ctx, in)
}

// authOptions is the bag populated by AuthOption functions and read
// by RequireAuth at request time. Keeping it private constrains the
// API to the WithX constructors below.
type authOptions struct {
	pats PATValidator
}

// AuthOption customises RequireAuth. Default behaviour (no options)
// is JWT-only: PAT-shaped bearers fall through to the JWT validator
// and 401 cleanly. Routes that want to accept PATs must opt in.
type AuthOption func(*authOptions)

// WithPAT enables PAT acceptance on the middleware instance. When a
// request arrives with an Authorization bearer that starts with
// IsPATFormat (i.e. mko_tkn_*), the middleware routes it to the
// supplied validator instead of the JWT path.
//
// Sensitive routes (PAT-management mutations, account deletion, etc.)
// achieve "PAT denied" by simply not passing this option -- safe by
// default, no DenyPAT escape hatch needed.
func WithPAT(p PATValidator) AuthOption {
	return func(o *authOptions) { o.pats = p }
}

// RequireAuth returns Echo middleware that validates the bearer in
// the Authorization header, resolves the account, and attaches it to
// the request context.
//
// Default (no opts): only Cognito JWTs are accepted. Existing
// callsites that pass two args remain identical to today.
//
// With WithPAT(...): PAT-shaped bearers (mko_tkn_*) are validated
// against the supplied PATValidator; everything else still goes to
// JWT. The dispatch is by prefix, so a JWT that happens to start
// with mko_tkn_ would (defensively) take the PAT path -- but JWT
// segment-zero is a base64url JSON header, never our prefix.
//
// Protected handlers retrieve the account via:
//
//	account, ok := c.Get(auth.ContextKeyAccount).(*models.Account)
//
// And inspect the credential via:
//
//	method, _ := c.Get(auth.ContextKeyAuthMethod).(string) // "cognito" | "pat"
//	patID,  _ := c.Get(auth.ContextKeyPATID).(string)      // "" on JWT path
//	scopes, _ := c.Get(auth.ContextKeyPATScopes).([]string) // ["*"] on JWT path
func RequireAuth(
	validator *JWTValidator,
	accounts AccountLookup,
	opts ...AuthOption,
) echo.MiddlewareFunc {
	o := &authOptions{}
	for _, fn := range opts {
		fn(o)
	}
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

			if o.pats != nil && IsPATFormat(tokenStr) {
				return authViaPAT(c, tokenStr, o.pats, accounts, next)
			}
			return authViaJWT(c, tokenStr, validator, accounts, next)
		}
	}
}

// authViaJWT is the original JWT-only RequireAuth body, factored out
// so RequireAuth can dispatch on bearer prefix without duplicating
// logic. Behaviour is byte-for-byte identical to pre-PAT RequireAuth
// modulo two extra context keys (auth_method=cognito, pat_scopes=["*"])
// added so downstream RequireScope checks bypass cleanly.
func authViaJWT(
	c echo.Context,
	tokenStr string,
	validator *JWTValidator,
	accounts AccountLookup,
	next echo.HandlerFunc,
) error {
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
	c.Set(ContextKeyAuthMethod, AuthMethodCognito)
	c.Set(ContextKeyPATScopes, []string{"*"})

	slog.Debug("auth succeeded",
		"account_id", account.ID,
		"cognito_sub", claims.Sub,
		"auth_method", AuthMethodCognito,
		"path", c.Path(),
	)
	return next(c)
}

// authViaPAT runs when the bearer is PAT-shaped and the route opted
// into PATs via WithPAT. It calls the validator, classifies the
// verdict, looks up the bound account by user_id, and populates the
// request context with the same shape JWT requests get plus the
// PAT-specific keys (pat_id, pat_scopes, auth_method=pat).
//
// Three failure modes intentionally collapse to 401 (we don't want to
// leak whether a token exists vs is revoked vs is expired):
//   - validator says invalid (any reason) -> 401, Warn(reason).
//   - account lookup returns nil/err     -> 401 (PAT outlived account).
//
// Storage failure on the validator path returns 500 so the MCP server
// can retry instead of pretending the token is bad.
func authViaPAT(
	c echo.Context,
	tokenStr string,
	pats PATValidator,
	accounts AccountLookup,
	next echo.HandlerFunc,
) error {
	verdict, err := pats.ValidatePAT(c.Request().Context(), PATValidateInput{
		Plaintext:      tokenStr,
		ActorIP:        clientIPFromContext(c),
		ActorUserAgent: c.Request().UserAgent(),
	})
	if err != nil {
		slog.Error("pat validator unavailable",
			"path", c.Path(),
			"error", err,
		)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"data": nil,
			"error": map[string]any{
				"code":    "INTERNAL_ERROR",
				"message": "validator unavailable",
			},
		})
	}
	if verdict == nil || !verdict.Valid {
		reason := ""
		if verdict != nil {
			reason = verdict.Reason
		}
		slog.Warn("pat rejected",
			"path", c.Path(),
			"reason", reason,
		)
		return authError(c, "invalid or expired token")
	}

	account, err := accounts.GetByID(c.Request().Context(), verdict.UserID)
	if err != nil {
		slog.Error("pat account lookup failed",
			"path", c.Path(),
			"user_id", verdict.UserID,
			"pat_id", verdict.TokenID,
			"error", err,
		)
		return authError(c, "account lookup failed")
	}
	if account == nil {
		slog.Warn("pat references missing account",
			"user_id", verdict.UserID,
			"pat_id", verdict.TokenID,
		)
		return authError(c, "invalid or expired token")
	}

	c.Set(ContextKeyAccount, account)
	c.Set(ContextKeyCognitoSub, account.CognitoSub)
	c.Set(ContextKeyAuthMethod, AuthMethodPAT)
	c.Set(ContextKeyPATID, verdict.TokenID)
	c.Set(ContextKeyPATScopes, verdict.Scopes)

	slog.Debug("auth succeeded",
		"account_id", account.ID,
		"auth_method", AuthMethodPAT,
		"pat_id", verdict.TokenID,
		"scopes", verdict.Scopes,
		"path", c.Path(),
	)
	return next(c)
}

// clientIPFromContext mirrors handlers.clientIP without the package
// dependency cycle. XFF first hop preferred, then RemoteAddr.
func clientIPFromContext(c echo.Context) string {
	if xff := strings.TrimSpace(c.Request().Header.Get("X-Forwarded-For")); xff != "" {
		if comma := strings.Index(xff, ","); comma >= 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return xff
	}
	addr := c.Request().RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// DatapackOwnershipLookup abstracts datapack lookup for the ownership
// middleware. Typically implemented by repository.DatapackRepo.
type DatapackOwnershipLookup interface {
	GetByID(userID, datapackID string) (*models.Datapack, error)
}

// RequireDatapackOwnership returns Echo middleware that verifies the
// authenticated user owns the datapack identified by the :datapack_id route
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

			datapackID := strings.TrimSpace(c.Param("datapack_id"))
			if datapackID == "" {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "datapack_id is required",
					},
				})
			}
			if _, parseErr := uuid.Parse(datapackID); parseErr != nil {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "invalid datapack_id format",
					},
				})
			}

			dp, err := datapacks.GetByID(account.ID, datapackID)
			if err != nil {
				slog.Error("ownership lookup failed",
					"datapack_id", datapackID, "user_id", account.ID, "error", err)
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
						"message": fmt.Sprintf("datapack %q not found", datapackID),
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
	GetByID(userID, datapackID string) (*models.Datapack, error)
	GetByIDForSharedAccess(userID, datapackID string) (*models.Datapack, error)
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

			datapackID := strings.TrimSpace(c.Param("datapack_id"))
			if datapackID == "" {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "datapack_id is required",
					},
				})
			}
			if _, parseErr := uuid.Parse(datapackID); parseErr != nil {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "VALIDATION_ERROR",
						"message": "invalid datapack_id format",
					},
				})
			}

			dp, err := datapacks.GetByID(account.ID, datapackID)
			if err != nil {
				slog.Error("access lookup failed",
					"datapack_id", datapackID, "user_id", account.ID, "error", err)
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

			dp, err = datapacks.GetByIDForSharedAccess(account.ID, datapackID)
			if err != nil {
				slog.Error("shared access lookup failed",
					"datapack_id", datapackID, "user_id", account.ID, "error", err)
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
						"message": fmt.Sprintf("datapack %q not found", datapackID),
					},
				})
			}

			c.Set(ContextKeyDatapack, dp)
			c.Set(ContextKeyAccessLevel, "shared")
			return next(c)
		}
	}
}

// RequireDatapackOwnerOrMaintainerGrant runs after RequireDatapackAccess. It
// allows only callers whose resolved datapack grant is owner or maintainer
// (e.g. outgoing-shares pending invites, invite/resend/revoke). Viewer and contributor receive 403.
//
// Ordering: this middleware only reads ContextKeyDatapack via GetDatapack; it
// does not load the datapack from the DB. RequireDatapackAccess (or
// RequireDatapackOwnership) must run first to populate that context. Reversing
// the chain would leave no datapack in context and fail after access checks.
func RequireDatapackOwnerOrMaintainerGrant() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			dp, err := GetDatapack(c)
			if err != nil || dp == nil {
				slog.Error("datapack missing after access middleware", "path", c.Path())
				return c.JSON(http.StatusInternalServerError, map[string]any{
					"data": nil,
					"error": map[string]any{
						"code":    "INTERNAL_ERROR",
						"message": "datapack context missing",
					},
				})
			}
			if models.IsOwnerOrMaintainerGrant(dp.Grant) {
				return next(c)
			}
			return c.JSON(http.StatusForbidden, map[string]any{
				"data": nil,
				"error": map[string]any{
					"code":    "FORBIDDEN",
					"message": "only datapack owners and maintainers are allowed",
				},
			})
		}
	}
}

// RequireScope returns middleware that enforces the request bears at
// least the given scope.
//
// Match rule (must run after RequireAuth):
//
//   - The scope set is read from ContextKeyPATScopes.
//   - If it contains "*" (wildcard) the request passes.
//   - Otherwise it must literally contain `required`.
//
// JWT-authenticated requests always carry pat_scopes=["*"] (set by
// authViaJWT) so this middleware is a no-op for human sessions and
// is safe to apply to routes that accept both bearer types.
//
// Mismatch returns 403 with code=INSUFFICIENT_SCOPE so callers can
// distinguish "you're authenticated but not authorised for this op"
// from a 401.
func RequireScope(required string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			scopes, _ := c.Get(ContextKeyPATScopes).([]string)
			for _, s := range scopes {
				if s == "*" || s == required {
					return next(c)
				}
			}
			method, _ := c.Get(ContextKeyAuthMethod).(string)
			patID, _ := c.Get(ContextKeyPATID).(string)
			slog.Warn("scope check failed",
				"path", c.Path(),
				"required", required,
				"have", scopes,
				"auth_method", method,
				"pat_id", patID,
			)
			return c.JSON(http.StatusForbidden, map[string]any{
				"data": nil,
				"error": map[string]any{
					"code":    "INSUFFICIENT_SCOPE",
					"message": fmt.Sprintf("token lacks required scope %q", required),
				},
			})
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
			// With Cognito off there is no email to verify, and the SPA's
			// onboarding step blocks on an unverified email (an OTP flow that
			// only Cognito can service). Treat the single dev account as
			// verified-at-creation so the UI is usable end to end. In-memory
			// only: the /auth/me handler reads email_verified from the context
			// account, and DevAuth runs on every request.
			if account.EmailVerifiedAt == nil {
				account.EmailVerifiedAt = &account.CreatedAt
			}
			// Likewise give the dev account a name so the SPA's name-completion
			// modal (also part of onboarding) doesn't block the page.
			if account.FirstName == "" {
				account.FirstName = "Dev"
			}
			if account.LastName == "" {
				account.LastName = "User"
			}
			c.Set(ContextKeyAccount, account)
			c.Set(ContextKeyCognitoSub, devSub)
			c.Set(ContextKeyAuthMethod, AuthMethodCognito)
			c.Set(ContextKeyPATScopes, []string{"*"})
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
