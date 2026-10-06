package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
	"meko/api_server/internal/telemetry"
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

	// Populate telemetry fields from the auth context. The share
	// endpoint's middleware admits owner OR maintainer, so a request
	// that reaches here is one or the other. ActingUserID is required
	// for correct Rudderstack attribution: the previous version fired
	// under the datapack owner's identity even when a maintainer sent
	// the invite, so the event landed on the wrong Rudderstack /
	// HubSpot user.
	if account, aerr := auth.GetAccount(c); aerr == nil && account != nil {
		req.ActingUserID = account.ID
		req.SharedByEmail = account.Email
		if account.ID == dp.UserID {
			req.SharedByRole = "owner"
		} else {
			req.SharedByRole = "maintainer"
		}
	}
	// POST /datapacks/:id/share is on the PAT-accepting dpDirect group
	// too, so a hardcoded UI tag would mis-slice every PAT/MCP share.
	// Derive from the auth method (JWT → UI, PAT → mcp/api depending
	// on the client hint).
	authMethod, _ := c.Get(auth.ContextKeyAuthMethod).(string)
	req.Source = telemetry.SourceFromAuthMethod(authMethod, c.Request().Header.Get(clientHintHeader))

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
		slog.Error("invite failed",
			"datapack_id", dp.ID, "recipient_email", req.RecipientEmail, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to send invite, please try again or contact support")
	}

	// Attribute the invite to the credential, not just the account: this
	// path provisions a Cognito user and mails a temporary password when
	// the recipient is new, so a disputed invite has to point at one
	// revocable key.
	slog.Info("invite sent",
		"datapack_id", dp.ID,
		"recipient_email", req.RecipientEmail,
		"grant_role", req.GrantRole,
		"auth_method", auth.GetAuthMethod(c),
		"pat_id", auth.GetPATID(c),
	)

	return ok(c, http.StatusOK, resp)
}

// maxBulkInviteEmails caps a single bulk-invite batch. Each row can fan out to
// a Cognito CreateUser plus an SES send — roughly 300-700 ms per row in serial.
// api_server itself sets no HTTP server timeout (see internal/api/server.go);
// the only ceiling in the path is the nginx proxy_read_timeout of 180 s
// configured on the meko-ui ingress (deploy/helm/meko/templates/meko-ui/
// nginx-configmap.yaml). 50 rows worst case is ~35 s — comfortably under, and
// matches the sync-invite caps at GitHub org invites, 1Password Business, and
// Slack. Larger batches belong on an async queued flow; raise this cap only
// if the invite path moves off the request goroutine.
const maxBulkInviteEmails = 50

// BulkInvite handles POST /api/v1/datapacks/:datapack_id/bulk-share.
//
// Partial success by design: the response is 200 with a per-email `results`
// array and a `summary` count per status. Only a malformed body, an empty list,
// an oversized batch, or an invalid shared grant fail the whole request.
//
//	@Summary	Invite many people to a datapack in one call
//	@Tags		sharing
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string						true	"Datapack ID"
//	@Param		body		body		service.BulkInviteRequest	true	"Bulk invite request"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/bulk-share [post]
func (h *SharingHandler) BulkInvite(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing datapack context")
	}

	var req service.BulkInviteRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if len(req.Emails) == 0 {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "emails is required and must contain at least one address")
	}
	if len(req.Emails) > maxBulkInviteEmails {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("too many emails: %d (max %d per request)", len(req.Emails), maxBulkInviteEmails))
	}

	// Populate telemetry fields from the auth context — same as the
	// single-invite handler above, propagated into each per-row
	// InviteRequest the service's loop builds. Without this, every
	// bulk-share landed as source=api (so the service's UI-skip gate
	// didn't fire and browser shares double-counted), and the event
	// was credited to the datapack owner instead of the acting
	// maintainer. See sharing_service.BulkInviteRequest.
	if account, aerr := auth.GetAccount(c); aerr == nil && account != nil {
		req.ActingUserID = account.ID
		req.SharedByEmail = account.Email
		if account.ID == dp.UserID {
			req.SharedByRole = "owner"
		} else {
			req.SharedByRole = "maintainer"
		}
	}
	authMethod, _ := c.Get(auth.ContextKeyAuthMethod).(string)
	req.Source = telemetry.SourceFromAuthMethod(authMethod, c.Request().Header.Get(clientHintHeader))

	// Same as Invite: invite rows use the datapack row owner as OwnerUserID so
	// resend/revoke checks match; maintainers act on behalf of the same datapack.
	resp, err := h.svc.BulkInviteToDatapack(
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
		// Per-row failures never surface here — they're classified into
		// results[]. Only the up-front whole-batch validation OR a
		// request-context cancellation can error.
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Client disconnect or upstream (nginx) timeout. The service
			// already stopped provisioning further users; whatever
			// reached results[] before cancellation is the partial state
			// we have. Log at info — this isn't a server bug.
			slog.Info("bulk invite: context cancelled mid-batch",
				"datapack_id", dp.ID,
				"requested", len(req.Emails),
				"processed", len(resp.Results),
				"error", err)
			// echo's default handler will short-circuit on a dead
			// connection anyway; returning the ctx error keeps the
			// signal for the framework to close cleanly.
			return err
		}
		slog.Error("bulk invite failed",
			"datapack_id", dp.ID, "email_count", len(req.Emails), "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to send invites, please try again or contact support")
	}

	// Same attribution as the single invite, and more load-bearing here: one
	// call can provision up to maxBulkInviteEmails Cognito users and mail that
	// many temporary passwords, so a disputed batch has to point at one
	// revocable key. Counts rather than addresses — the per-row outcomes are
	// in the response, and the recipient list does not belong in the log.
	slog.Info("bulk invite sent",
		"datapack_id", dp.ID,
		"grant_role", req.GrantRole,
		"requested", len(req.Emails),
		"results", len(resp.Results),
		"auth_method", auth.GetAuthMethod(c),
		"pat_id", auth.GetPATID(c),
	)

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
//	@Failure	401		{object}	APIResponse
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

	// The recipient is the CALLER, never whatever the body claims. The
	// service only checks recipient_user_id against the invite, so without
	// this any authenticated caller holding an invite_id and the real
	// recipient's user_id could accept or decline on their behalf -- and
	// GET /datapacks/:id/outgoing-shares hands both to every maintainer.
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	req.RecipientUserID = account.ID

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
		// Same rule as Invite above: this path can return a raw Cognito/AWS
		// SDK error (e.g. AccessDeniedException with IAM ARNs and request
		// IDs) via ResetTempPassword — never echo err.Error() to the client.
		slog.Error("resend invite failed",
			"datapack_id", dp.ID, "invite_id", inviteID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to resend invite, please try again or contact support")
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
