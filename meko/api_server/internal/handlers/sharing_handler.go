package handlers

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// SharingHandler holds Echo handlers for datapack sharing endpoints.
type SharingHandler struct {
	svc *service.SharingService
}

// NewSharingHandler creates a new SharingHandler.
func NewSharingHandler(svc *service.SharingService) *SharingHandler {
	return &SharingHandler{svc: svc}
}

// Invite handles POST /api/v1/datapacks/:datapack_id/share.
//
//	@Summary	Invite a person to a datapack
//	@Tags		sharing
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string				true	"Datapack ID"
//	@Param		body	body		service.InviteRequest	true	"Invite request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/share [post]
func (h *SharingHandler) Invite(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	var req service.InviteRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	// Invite rows use the datapack row owner as OwnerUserID so resend/revoke
	// checks match; maintainers act on behalf of the same datapack.
	resp, err := h.svc.InviteToDatapack(
		c.Request().Context(),
		dp.ID,
		dp.UserID,
		dp.Name,
		dp.Description,
		dp.OwnerName,
		dp.OwnerEmail,
		req,
	)
	if err != nil {
		if isValidationErr(err) || errors.Is(err, service.ErrCannotInviteOwner) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, repository.ErrInviteExists) {
			return apiError(c, http.StatusConflict,
				"CONFLICT", err.Error())
		}
		if errors.Is(err, repository.ErrAlreadyMember) {
			return apiError(c, http.StatusConflict,
				"ALREADY_MEMBER", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, resp)
}

// IncomingShares handles GET /api/v1/datapacks/incoming-shares.
//
//	@Summary	List incoming share invites for the authenticated user
//	@Tags		sharing
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/incoming-shares [get]
func (h *SharingHandler) IncomingShares(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	shares, err := h.svc.ListIncomingShares(account.Email)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, shares)
}

// InviteAction handles POST /api/v1/datapacks/invite-action.
//
//	@Summary	Accept or decline a pending invite
//	@Tags		sharing
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.InviteActionRequest	true	"Action request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	403		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/invite-action [post]
func (h *SharingHandler) InviteAction(c echo.Context) error {
	var req service.InviteActionRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	resp, err := h.svc.HandleInviteAction(req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		if isForbiddenErr(err) {
			return apiError(c, http.StatusForbidden,
				"FORBIDDEN", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, resp)
}

// OutgoingShares handles GET /api/v1/datapacks/:datapack_id/outgoing-shares (pending invites only).
//
//	@Summary	List pending outgoing invites for a datapack
//	@Tags		sharing
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	403		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/outgoing-shares [get]
func (h *SharingHandler) OutgoingShares(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	resp, err := h.svc.ListOutgoingPendingInvites(dp.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, resp)
}

// MemberList handles GET /api/v1/datapacks/:datapack_id/member-list (accepted members).
//
//	@Summary	List datapack members
//	@Tags		sharing
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/member-list [get]
func (h *SharingHandler) MemberList(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	resp, err := h.svc.ListDatapackMembers(dp.ID, dp.UserID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, resp)
}

// ResendInvite handles POST /api/v1/datapacks/:datapack_id/invite/:invite_id/resend.
//
//	@Summary	Resend a pending invite
//	@Tags		sharing
//	@Produce	json
//	@Param		datapack_id		path		string	true	"Datapack ID"
//	@Param		invite_id	path		string	true	"Invite ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/invite/{invite_id}/resend [post]
func (h *SharingHandler) ResendInvite(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	inviteID := c.Param("invite_id")
	if inviteID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "invite_id is required")
	}

	id, err := h.svc.ResendInvite(
		c.Request().Context(),
		dp.ID,
		dp.UserID,
		dp.Name,
		dp.Description,
		dp.OwnerName,
		dp.OwnerEmail,
		inviteID,
	)
	if err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"status":    "resent",
		"invite_id": id,
	})
}

// RevokeInvite handles DELETE /api/v1/datapacks/:datapack_id/invite/:invite_id.
//
//	@Summary	Revoke a pending invite
//	@Tags		sharing
//	@Produce	json
//	@Param		datapack_id		path		string	true	"Datapack ID"
//	@Param		invite_id	path		string	true	"Invite ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/invite/{invite_id} [delete]
func (h *SharingHandler) RevokeInvite(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	inviteID := c.Param("invite_id")
	if inviteID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "invite_id is required")
	}

	if err := h.svc.RevokeInvite(dp.ID, dp.UserID, inviteID); err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"status":    "revoked",
		"invite_id": inviteID,
	})
}

// UpdateGrant handles PATCH /api/v1/datapacks/:datapack_id/share/:share_user_id.
//
//	@Summary	Update a member's grant level
//	@Tags		sharing
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id			path		string					true	"Datapack ID"
//	@Param		share_user_id	path		string					true	"Member user ID"
//	@Param		body			body		service.UpdateGrantRequest	true	"New grant"
//	@Success	200				{object}	APIResponse
//	@Failure	400				{object}	APIResponse
//	@Failure	404				{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/share/{share_user_id} [patch]
func (h *SharingHandler) UpdateGrant(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	shareUserID := c.Param("share_user_id")
	if shareUserID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "share_user_id is required")
	}

	var req service.UpdateGrantRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	resp, err := h.svc.UpdateMemberGrant(dp.ID, dp.UserID, shareUserID, req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, resp)
}

func isForbiddenErr(err error) bool {
	return containsStr(err.Error(), "does not match")
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
