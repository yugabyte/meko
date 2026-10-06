// Package handlers provides Echo request handlers for the datapack API.
package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/service"
)

// clientHintHeader lets callers self-identify for Rudderstack's `source`
// prop when the auth method alone is ambiguous. The hint takes precedence
// over the auth method inside telemetry.SourceFromAuthMethod — important
// because meko-mcp-server's MCP tools swap their fastmcp OAuth-proxy
// token for the user's upstream Cognito access token before calling
// api_server, so a Cognito JWT alone can't distinguish a browser from
// an MCP tool call. Without the hint, Cognito → UI, PAT → API.
//
// meko-mcp-server is the only caller intended to send this today, with
// value "mcp"; the companion PR there sets the header on every outgoing
// api_server call. Until that lands, every PAT-driven MCP operation
// shows up as source=api in the funnel — the plumbing is here, the
// signal only starts flowing once the client sends it.
const clientHintHeader = "X-Meko-Client"

// APIResponse is the standard envelope for all API responses. Error is omitted on
// success rather than serialized as null, so its presence in a response is a
// reliable failure signal.
type APIResponse struct {
	Data  any       `json:"data"`
	Error *APIError `json:"error,omitempty"`
}

// APIError represents a structured error in the API response.
type APIError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// ok sends a success JSON response.
func ok(c echo.Context, status int, data any) error {
	return c.JSON(status, APIResponse{Data: data})
}

// apiError sends an error JSON response.
func apiError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, APIResponse{
		Error: &APIError{Code: code, Message: message},
	})
}

// writeAndFail writes the given error response via apiError and returns a
// real, non-nil sentinel independent of apiError's own return value.
// apiError returns c.JSON's encode/write error, which is nil on any
// successful write regardless of `status` -- so a caller checking
// `if _, err := someGate(...); err != nil { return err }` never actually
// stopped (MEKO-656 / MEKO-690, see datapack_access.go's RequireDatapackAccess
// and auth_handler.go's extractAccessToken): the error body was written, but
// execution fell through into the real query/mutation, which then wrote a
// second, real response onto the same already-committed one. Since the
// response is already committed by the time this returns, the sentinel never
// reaches CustomHTTPErrorHandler's own write path below -- it checks
// c.Response().Committed and no-ops.
//
// bindJSON/resolveAgent predate this and solve the same problem a different
// way (returning bool, not error -- see bind.go's own comment on bindJSON).
// Either shape is fine for new code; what doesn't work is returning
// apiError's own result and trusting it as a signal.
func writeAndFail(c echo.Context, status int, code, message string) error {
	_ = apiError(c, status, code, message)
	return echo.NewHTTPError(status, message)
}

// respondTierLimit sends the standard 402 for a tier-cap hit (datapack, KB
// storage, AMP compute, …). The (code, message) come from the service's
// per-tier dispatch (DatapackLimitResponse / KBStorageLimitResponse /
// CoreLimitResponse), so every cap site returns the same shape instead of
// re-spelling the StatusPaymentRequired call.
func respondTierLimit(c echo.Context, resp service.TierLimitResponse) error {
	return apiError(c, http.StatusPaymentRequired, resp.Code, resp.Message)
}

// CustomHTTPErrorHandler provides a uniform JSON error envelope for Echo
// errors (404, 405, etc.).
func CustomHTTPErrorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	code := http.StatusInternalServerError
	msg := "internal server error"

	if he, isHTTPErr := err.(*echo.HTTPError); isHTTPErr {
		code = he.Code
		if m, isStr := he.Message.(string); isStr {
			msg = m
		}
	}

	if jsonErr := c.JSON(code, APIResponse{
		Error: &APIError{Code: http.StatusText(code), Message: msg},
	}); jsonErr != nil {
		c.Logger().Error(jsonErr)
	}
}
