package handlers

import (
	"errors"
	"net/http"
	"strings"
	"log/slog"

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

	setAuthCookie(c, "access_token", resp.AccessToken)
	setAuthCookie(c, "id_token", resp.IDToken)
	setAuthCookie(c, "refresh_token", resp.RefreshToken)

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
	accessToken := ""
	if cookie, err := c.Cookie("access_token"); err == nil {
		accessToken = strings.TrimSpace(cookie.Value)
	}
	if accessToken == "" {
		header := c.Request().Header.Get("Authorization")
		if header != "" {
			parts := strings.SplitN(header, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				accessToken = strings.TrimSpace(parts[1])
			}
		}
	}
	if accessToken == "" {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "access token is required")
	}

	if err := h.svc.Logout(c.Request().Context(), accessToken); err != nil {
		return h.classifyAuthErr(c, err)
	}

	clearAuthCookie(c, "access_token")
	clearAuthCookie(c, "id_token")
	clearAuthCookie(c, "refresh_token")

	return ok(c, http.StatusOK, map[string]string{
		"message": "logged out",
	})
}

// Me handles GET /api/v1/auth/me.
func (h *AuthHandler) Me(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		slog.Error("error getting account", "error", err)
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	return ok(c, http.StatusOK, map[string]any{
		"account_id":               account.ID,
		"email":                    account.Email,
		"first_name":               account.FirstName,
		"last_name":                account.LastName,
		"has_completed_onboarding": account.HasCompletedOnboarding,
	})
}

// UpdateOnboardingStatus handles PATCH /api/v1/auth/me.
//
//	@Summary	Update onboarding completion status
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.UpdateOnboardingRequest	true	"Onboarding status payload"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Router		/auth/me [patch]
func (h *AuthHandler) UpdateOnboardingStatus(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	var req service.UpdateOnboardingRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.UpdateOnboardingStatus(
		c.Request().Context(),
		account.ID,
		req,
	); err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusOK, map[string]any{
		"message":                  "onboarding status updated",
		"has_completed_onboarding": req.HasCompletedOnboarding,
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

func setAuthCookie(c echo.Context, name, value string) {
	secure := c.Scheme() == "https"
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
	c.SetCookie(cookie)
}

func clearAuthCookie(c echo.Context, name string) {
	secure := c.Scheme() == "https"
	cookie := &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
	c.SetCookie(cookie)
}
