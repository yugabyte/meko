package handlers

import (
	"errors"
	"log/slog"
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

// ResendConfirmation handles POST /api/v1/auth/resend-confirmation.
// Triggers Cognito ResendConfirmationCode for users who signed up but
// never completed email verification (same app client as SignUp).
//
//	@Summary	Resend email verification code
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.ResendConfirmationRequest	true	"Email to resend code to"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	429		{object}	APIResponse
//	@Router		/auth/resend-confirmation [post]
func (h *AuthHandler) ResendConfirmation(c echo.Context) error {
	var req service.ResendConfirmationRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.ResendConfirmationEmail(c.Request().Context(), req); err != nil {
		return h.classifyAuthErr(c, err)
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "verification code sent",
	})
}

// ForgotPassword handles POST /api/v1/auth/forgot-password (Cognito ForgotPassword).
//
//	@Summary	Start password reset — email verification code
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.ForgotPasswordStartRequest	true	"Account email"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	429		{object}	APIResponse
//	@Router		/auth/forgot-password [post]
func (h *AuthHandler) ForgotPassword(c echo.Context) error {
	var req service.ForgotPasswordStartRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if err := h.svc.ForgotPasswordSendCode(c.Request().Context(), req); err != nil {
		return h.classifyAuthErr(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{
		"message": "If an account exists, a verification code has been sent.",
	})
}

// ForgotPasswordConfirm handles POST /api/v1/auth/forgot-password/confirm (Cognito ConfirmForgotPassword).
//
//	@Summary	Complete password reset with code and new password
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.ForgotPasswordConfirmRequest	true	"Reset payload"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	429		{object}	APIResponse
//	@Router		/auth/forgot-password/confirm [post]
func (h *AuthHandler) ForgotPasswordConfirm(c echo.Context) error {
	var req service.ForgotPasswordConfirmRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if err := h.svc.ConfirmForgotPasswordReset(c.Request().Context(), req); err != nil {
		return h.classifyAuthErr(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{
		"message": "password updated",
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
//	@Failure	403		{object}	APIResponse	"Cognito USER_NOT_CONFIRMED (code USER_NOT_CONFIRMED); client should offer resend / confirm flow"
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
//	@Security	BearerAuth
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
//
//	@Summary	Get the authenticated user
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/auth/me [get]
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
//	@Security	BearerAuth
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
	case errors.Is(err, service.ErrForgotPasswordNoAccount):
		return apiError(c, http.StatusNotFound,
			"NOT_FOUND", err.Error())
	case errors.Is(err, auth.ErrUserNotConfirmed):
		// Client: show resend + link to confirmation (sign-up) flow only for this code.
		return apiError(c, http.StatusForbidden,
			"USER_NOT_CONFIRMED",
			"Verify your email before signing in. You can resend the code or enter it on the confirmation screen.")
	case errors.Is(err, auth.ErrUserAlreadyConfirmed):
		// Resend was attempted on a verified account. 409 (state conflict)
		// lets the client switch the resend UI back to a sign-in nudge
		// instead of showing a generic validation error.
		return apiError(c, http.StatusConflict,
			"USER_ALREADY_CONFIRMED",
			"This email is already verified. Please sign in.")
	case errors.Is(err, auth.ErrInvalidCognitoParameter):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "invalid request parameters")
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
	case errors.Is(err, service.ErrPasswordTooShort),
		errors.Is(err, service.ErrPasswordMismatch):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", err.Error())
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
