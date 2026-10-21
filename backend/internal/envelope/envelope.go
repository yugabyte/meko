// Package envelope is the {data, error} reply every Meko Local API uses: the HTTP server's /api/v1,
// and the internal store API the engine calls.
package envelope

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Response is every reply's envelope, the same wire shape as prod Meko's API: data on success,
// error (and data null) on failure. error is omitted on success, so its presence means failure.
type Response struct {
	Data  any    `json:"data"`
	Error *Error `json:"error,omitempty"`
}

// Error is a failure's code (machine-readable) and message (for people).
type Error struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// OK replies 200 with data.
func OK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, Response{Data: data})
}

// Fail replies with status and an error.
func Fail(c echo.Context, status int, code, message string) error {
	return c.JSON(status, Response{Error: &Error{Code: code, Message: message}})
}

var statusCodes = map[int]string{
	http.StatusBadRequest:            "BAD_REQUEST",
	http.StatusUnauthorized:          "UNAUTHORIZED",
	http.StatusForbidden:             "FORBIDDEN",
	http.StatusNotFound:              "NOT_FOUND",
	http.StatusMethodNotAllowed:      "METHOD_NOT_ALLOWED",
	http.StatusRequestEntityTooLarge: "PAYLOAD_TOO_LARGE",
}

// ErrorHandler writes every error as an envelope. An HTTP error below 500 keeps its status and
// message; anything else (a 5xx HTTP error too) is logged and the caller gets a generic message,
// so internal details (paths, SQL) never reach a reply.
func ErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code < http.StatusInternalServerError {
			code, found := statusCodes[he.Code]
			if !found {
				code = "ERROR"
			}
			msg, isString := he.Message.(string)
			if !isString || msg == "" {
				msg = http.StatusText(he.Code)
			}
			_ = Fail(c, he.Code, code, msg)
			return
		}
		log.Error("request failed", "method", c.Request().Method, "route", c.Path(),
			"request_id", c.Response().Header().Get(echo.HeaderXRequestID), "error", err)
		_ = Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not complete the request. Please try again.")
	}
}
