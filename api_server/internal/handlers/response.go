// Package handlers provides Echo request handlers for the datapack API.
package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// APIResponse is the standard envelope for all API responses.
type APIResponse struct {
	Data  any       `json:"data"`
	Error *APIError `json:"error"`
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
