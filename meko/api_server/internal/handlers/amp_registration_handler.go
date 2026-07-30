// amp_registration_handler serves the admin AMP-controller-registration
// endpoints (/admin/amp-registration) consumed by meko_admin. Registration
// lives in meko_system.amp_registration; routing the writes through api_server
// (rather than meko_admin's own DB pool) keeps a single source of DB truth so
// the admin console can't drift onto a different database.
//
// NOTE: password is stored as-is for now (the AMP controller has no auth yet)
// and is never returned to the client. Move to a secret store / encrypt at rest
// before AMP enforces auth.
package handlers

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/repository"
)

// AMPRegistrationHandler serves admin registration CRUD.
type AMPRegistrationHandler struct {
	repo *repository.AMPRegistrationRepo
}

// NewAMPRegistrationHandler constructs an AMPRegistrationHandler.
func NewAMPRegistrationHandler(repo *repository.AMPRegistrationRepo) *AMPRegistrationHandler {
	return &AMPRegistrationHandler{repo: repo}
}

// ampRegBody is the register/update request shape (mirrors meko_admin's form).
type ampRegBody struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Get handles GET /admin/amp-registration. Returns {name, url, username} with
// data=null when AMP is not registered (meko_admin maps null to a 404 for its
// client) — never the password.
func (h *AMPRegistrationHandler) Get(c echo.Context) error {
	reg, err := h.repo.Get(c.Request().Context())
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if reg == nil {
		return ok(c, http.StatusOK, nil)
	}
	return ok(c, http.StatusOK, map[string]any{
		"name": reg.Name, "url": reg.BaseURL, "username": reg.Username,
	})
}

// Register handles POST /admin/amp-registration — register or replace the
// singleton controller row.
func (h *AMPRegistrationHandler) Register(c echo.Context) error {
	var body ampRegBody
	if err := c.Bind(&body); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_BODY", "invalid request body")
	}
	url := strings.TrimSpace(body.URL)
	if url == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "url is required")
	}
	reg := repository.AMPRegistration{
		Name:     strings.TrimSpace(body.Name),
		BaseURL:  url,
		Username: body.Username,
		Password: body.Password,
	}
	if err := h.repo.Upsert(c.Request().Context(), reg); err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusCreated, map[string]any{"name": reg.Name, "url": reg.BaseURL})
}

// Patch handles PATCH /admin/amp-registration — partial update; only string
// fields present in the body override the current value. 404 when unregistered.
func (h *AMPRegistrationHandler) Patch(c echo.Context) error {
	cur, err := h.repo.Get(c.Request().Context())
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if cur == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "AMP is not registered")
	}
	// Distinguish "field omitted" from "field set to empty": bind into a map so
	// only keys actually present override the current value.
	var raw map[string]any
	if err := c.Bind(&raw); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_BODY", "invalid request body")
	}
	str := func(key, fallback string) string {
		if v, ok := raw[key].(string); ok {
			return v
		}
		return fallback
	}
	next := repository.AMPRegistration{
		Name:     strings.TrimSpace(str("name", cur.Name)),
		BaseURL:  strings.TrimSpace(str("url", cur.BaseURL)),
		Username: str("username", cur.Username),
		Password: str("password", cur.Password),
	}
	if next.BaseURL == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "url cannot be empty")
	}
	if err := h.repo.Upsert(c.Request().Context(), next); err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"name": next.Name, "url": next.BaseURL})
}

// Delete handles DELETE /admin/amp-registration — unregister AMP.
func (h *AMPRegistrationHandler) Delete(c echo.Context) error {
	if err := h.repo.Delete(c.Request().Context()); err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"status": "unregistered"})
}
