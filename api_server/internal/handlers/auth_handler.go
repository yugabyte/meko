package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// AuthHandler holds Echo handlers for authentication endpoints.
type AuthHandler struct {
	svc *service.AuthService
}

// NewAuthHandler creates a new AuthHandler.
func NewAuthHandler(svc *service.AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Signup handles POST /api/v1/auth/signup.
//
//	@Summary	Register a new user
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.SignupRequest	true	"Signup request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Router		/auth/signup [post]
func (h *AuthHandler) Signup(c echo.Context) error {
	var req service.SignupRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	resp, err := h.svc.Signup(c.Request().Context(), req)
	if err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusCreated, resp)
}

// ConfirmSignup handles POST /api/v1/auth/confirm-signup.
//
//	@Summary	Confirm email with verification code
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.ConfirmSignupRequest	true	"Confirm request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Router		/auth/confirm-signup [post]
func (h *AuthHandler) ConfirmSignup(c echo.Context) error {
	var req service.ConfirmSignupRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.ConfirmSignup(c.Request().Context(), req); err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "email confirmed",
	})
}

// Login handles POST /api/v1/auth/login.
//
//	@Summary	Authenticate and receive tokens
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.LoginRequest	true	"Login request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Router		/auth/login [post]
func (h *AuthHandler) Login(c echo.Context) error {
	var req service.LoginRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	resp, err := h.svc.Login(c.Request().Context(), req)
	if err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusOK, resp)
}

// Logout handles POST /api/v1/auth/logout.
//
//	@Summary	Invalidate all sessions
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Router		/auth/logout [post]
func (h *AuthHandler) Logout(c echo.Context) error {
	header := c.Request().Header.Get("Authorization")
	if header == "" {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "authorization header is required")
	}

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "malformed authorization header")
	}

	accessToken := strings.TrimSpace(parts[1])
	if accessToken == "" {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "access token is empty")
	}

	if err := h.svc.Logout(c.Request().Context(), accessToken); err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "logged out",
	})
}

// classifyAuthErr maps auth/cognito sentinel errors to the appropriate
// HTTP status and error code.
func (h *AuthHandler) classifyAuthErr(
	c echo.Context, err error,
) error {
	switch {
	case errors.Is(err, auth.ErrUserExists):
		return apiError(c, http.StatusConflict,
			"CONFLICT", "user already exists")
	case errors.Is(err, auth.ErrNotAuthorized):
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "invalid credentials")
	case errors.Is(err, auth.ErrUserNotFound):
		return apiError(c, http.StatusNotFound,
			"NOT_FOUND", "user not found")
	case errors.Is(err, auth.ErrUserNotConfirmed):
		return apiError(c, http.StatusForbidden,
			"USER_NOT_CONFIRMED", "email not confirmed")
	case errors.Is(err, auth.ErrCodeMismatch):
		return apiError(c, http.StatusBadRequest,
			"INVALID_CODE", "invalid confirmation code")
	case errors.Is(err, auth.ErrCodeExpired):
		return apiError(c, http.StatusBadRequest,
			"CODE_EXPIRED", "confirmation code has expired")
	case errors.Is(err, auth.ErrInvalidPassword):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			"password does not meet requirements")
	case errors.Is(err, auth.ErrTooManyRequests),
		errors.Is(err, auth.ErrLimitExceeded):
		return apiError(c, http.StatusTooManyRequests,
			"RATE_LIMITED", "too many requests, try again later")
	case isValidationErr(err):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", err.Error())
	default:
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
}
