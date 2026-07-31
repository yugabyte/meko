package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
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
		if errors.As(err, &he) {
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
		} else {
			// Not an echo.HTTPError: an unexpected error escaped a controller.
			// Don't leak its detail to the caller, but record it.
			logger.LogAttrs(c.Request().Context(), slog.LevelError, "unhandled request error",
				slog.String("error", err.Error()))
		}

		if writeErr := c.JSON(status, apiError(codeForStatus(status), message)); writeErr != nil {
			logger.LogAttrs(c.Request().Context(), slog.LevelError, "failed to write error response",
				slog.String("error", writeErr.Error()))
		}
	}
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
