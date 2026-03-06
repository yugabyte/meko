// Package handlers provides HTTP request handlers for the datapack API.
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
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

// WriteJSON writes a success response with the given status code and data.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := APIResponse{Data: data}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to encode response", "error", err)
	}
}

// WriteError writes an error response with the given status code.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := APIResponse{
		Error: &APIError{
			Code:    code,
			Message: message,
		},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to encode error response", "error", err)
	}
}

// WriteErrorWithDetails writes an error response with additional detail strings.
func WriteErrorWithDetails(
	w http.ResponseWriter,
	status int,
	code, message string,
	details []string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := APIResponse{
		Error: &APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to encode error response", "error", err)
	}
}
