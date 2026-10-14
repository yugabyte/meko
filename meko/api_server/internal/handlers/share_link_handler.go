package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// ShareLinkHandler serves a datapack's share link: managing it, previewing it,
// and joining through it.
type ShareLinkHandler struct {
	svc *service.ShareLinkService
}

func NewShareLinkHandler(svc *service.ShareLinkService) *ShareLinkHandler {
	return &ShareLinkHandler{svc: svc}
}

// Get handles GET /api/v1/datapacks/:datapack_id/share-link.
//
//	@Summary	Get the datapack's share link (owner or maintainer)
//	@Tags		sharing
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse	"share_link is null when the datapack has none"
//	@Failure	403			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/share-link [get]
func (h *ShareLinkHandler) Get(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}
	link, err := h.svc.Get(dp.ID)
	if err != nil {
		slog.Error("get share link failed", "datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load share link")
	}
	return ok(c, http.StatusOK, map[string]any{"share_link": link})
}

// Update handles PUT /api/v1/datapacks/:datapack_id/share-link.
//
//	@Summary	Create the datapack's share link, or change its role or status (owner or maintainer)
//	@Tags		sharing
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string							true	"Datapack ID"
//	@Description	reset: true replaces the token, so the old URL stops working.
//	@Param		body		body		service.UpdateShareLinkRequest	true	"grant: viewer|contributor; status: active|off"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	403			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/share-link [put]
func (h *ShareLinkHandler) Update(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	var req service.UpdateShareLinkRequest
	if err = c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	link, err := h.svc.Update(dp.ID, account.ID, req)
	if err != nil {
		if errors.Is(err, service.ErrInvalidShareLink) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		slog.Error("update share link failed", "datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update share link")
	}
	return ok(c, http.StatusOK, map[string]any{"share_link": link})
}

// Preview handles GET /api/v1/share/:token.
//
//	@Summary	Preview the datapack behind a share link
//	@Description	Only the datapack's name and description. An unknown token and a link that is off are both 404.
//	@Tags		sharing
//	@Produce	json
//	@Param		token	path		string	true	"Share link token"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/share/{token} [get]
func (h *ShareLinkHandler) Preview(c echo.Context) error {
	p, err := h.svc.Preview(c.Param("token"))
	if err != nil {
		return h.linkError(c, err, "preview")
	}
	return ok(c, http.StatusOK, p)
}

// Redeem handles POST /api/v1/share/:token/redeem.
//
//	@Summary	Join the datapack behind a share link
//	@Description	Never lowers the caller's role. A pending invite is accepted with the higher of
//	@Description	its role and the link's. Someone an owner removed is refused until invited again by email.
//	@Tags		sharing
//	@Produce	json
//	@Param		token	path		string	true	"Share link token"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	403		{object}	APIResponse	"REMOVED_FROM_DATAPACK: the caller was removed"
//	@Failure	404		{object}	APIResponse
//	@Failure	503		{object}	APIResponse	"SHARE_LINK_BUSY: too many concurrent joins; retry after Retry-After"
//	@Security	BearerAuth
//	@Router		/share/{token}/redeem [post]
func (h *ShareLinkHandler) Redeem(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	res, err := h.svc.Redeem(c.Param("token"), account)
	if err != nil {
		return h.linkError(c, err, "redeem")
	}
	return ok(c, http.StatusOK, res)
}

func (h *ShareLinkHandler) linkError(c echo.Context, err error, op string) error {
	if errors.Is(err, repository.ErrShareLinkNotFound) {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "this link isn't available")
	}
	if errors.Is(err, repository.ErrRemovedFromDatapack) {
		return apiError(c, http.StatusForbidden, "REMOVED_FROM_DATAPACK",
			"you were removed from this datapack; ask its owner to invite you again")
	}
	if errors.Is(err, repository.ErrShareLinkBusy) {
		c.Response().Header().Set("Retry-After", "1")
		return apiError(c, http.StatusServiceUnavailable, "SHARE_LINK_BUSY",
			"many people are joining through this link right now; try again in a moment")
	}
	slog.Error("share link "+op+" failed", "error", err)
	return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to "+op+" share link")
}
