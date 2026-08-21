package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/amp/controller/internal/db"
)

// apiError builds the spec's Error envelope (schema Error -> {error:{code,message}}).
func apiError(code, message string) Error {
	return Error{Error: ErrorBody{Code: code, Message: message}}
}

// errorHandler renders every framework-level error (route 404/405, body-bind
// 400, panics recovered to 500, or an error returned by a controller) using the
// same Error envelope the handlers return, so the wire shape is uniform.
func errorHandler(logger *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}

		status := http.StatusInternalServerError
		message := http.StatusText(status)

		var he *echo.HTTPError
		switch {
		case errors.As(err, &he):
			status = he.Code
			// he.Message is `any`: echo sets it to a string for most errors, but
			// to an error or a struct for some (e.g. binding/validation failures).
			// Preserve the detail in all cases instead of dropping it.
			switch m := he.Message.(type) {
			case string:
				message = m
			case error:
				message = m.Error()
			default:
				message = fmt.Sprintf("%v", he.Message)
			}

		case replayable(err, c.Request().Method):
			// The state store was unreachable and this request can be replayed, so
			// answer with the retryable status instead of the catch-all 500 the
			// caller cannot distinguish from a controller bug (#886). Not the same
			// test /readyz runs: that one is a bare Ping where any failure means
			// unhealthy, while this classifies the error a real statement returned,
			// so the two can disagree at the edges (a pool acquire that times out is
			// 503 on /readyz and 500 here).
			status = http.StatusServiceUnavailable
			message = "state store unavailable; retry the request"
			c.Response().Header().Set(echo.HeaderRetryAfter, strconv.Itoa(retryAfterSeconds))
			logger.LogAttrs(c.Request().Context(), slog.LevelWarn, "state store unavailable",
				slog.String("error", err.Error()))

		default:
			// Not an echo.HTTPError: an unexpected error escaped a controller.
			// Don't leak its detail to the caller, but record it. A store outage
			// that reaches here is one whose request is not safely replayable, so
			// it stays a 500 — say which it was, since the two want different
			// operator reactions.
			msg := "unhandled request error"
			if db.Unavailable(err) {
				msg = "state store unavailable mid-request; outcome unknown, not reported as retryable"
			}
			logger.LogAttrs(c.Request().Context(), slog.LevelError, msg,
				slog.String("error", err.Error()))
		}

		if writeErr := c.JSON(status, apiError(codeForStatus(status), message)); writeErr != nil {
			logger.LogAttrs(c.Request().Context(), slog.LevelError, "failed to write error response",
				slog.String("error", writeErr.Error()))
		}
	}
}

// retryAfterSeconds is the Retry-After hint every 503 carries. What a 503 waits
// on — a tserver restart, an endpoint's safekeeper coming up — clears in
// seconds, so this is short enough to be useful and long enough not to turn a
// polling client into a hot loop.
const retryAfterSeconds = 2

// replayable reports whether err is the state store being unreachable *and* the
// request can be replayed as-is. An error the store marked as following a
// durable write never can, whatever the method. Otherwise a read always can, and
// a mutation only when the failure provably preceded the statement reaching the
// store — one that died mid-mutation may have committed, so telling the caller to
// retry could double-apply the write.
func replayable(err error, method string) bool {
	if !db.Unavailable(err) || errors.Is(err, db.ErrOutcomeUnknown) {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	}
	return db.SafeToRetry(err)
}

// codeForStatus maps an HTTP status to a stable, machine-readable error code.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusInternalServerError:
		return "internal"
	default:
		return strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")
	}
}
