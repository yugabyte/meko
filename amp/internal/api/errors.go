package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ErrorResponse is the controller's JSON error envelope. Every error the API
// returns uses this shape.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries a stable machine-readable code and a human-readable message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}

// notImplemented is the placeholder response for stub mutation handlers.
func notImplemented(c echo.Context, what string) error {
	return writeError(c, http.StatusNotImplemented, "not_implemented", what+" is not implemented yet")
}
