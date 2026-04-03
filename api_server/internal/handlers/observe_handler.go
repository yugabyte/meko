package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// ObserveHandler handles lightweight Observe bootstrap.
type ObserveHandler struct {
	langfuse *service.LangfuseService
}

func NewObserveHandler(langfuse *service.LangfuseService) *ObserveHandler {
	return &ObserveHandler{langfuse: langfuse}
}

// BootstrapSession handles POST /api/v1/observe/session/bootstrap.
// It establishes a Langfuse web session by forwarding upstream Set-Cookie
// headers from CSRF + credentials login flow back to the browser.
func (h *ObserveHandler) BootstrapSession(c echo.Context) error {
	if h.langfuse == nil || !h.langfuse.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"SERVICE_UNAVAILABLE",
			"langfuse is not configured")
	}
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	result, err := h.langfuse.BootstrapObserveSession(
		account.ID,
		account.Email,
		account.FirstName,
		account.LastName,
	)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	for _, rawCookie := range result.SetCookies {
		c.Response().Header().Add(echo.HeaderSetCookie, rawCookie)
	}
	return ok(c, http.StatusOK, map[string]string{
		"message": "observe bootstrap ready",
		"org_id":  result.OrgID,
	})
}
