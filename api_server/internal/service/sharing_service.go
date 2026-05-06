package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// CognitoInviter abstracts the Cognito AdminCreateUser call used to
// send signup invitation emails to non-existing users.
type CognitoInviter interface {
	InviteUser(ctx context.Context, email string, resend bool) error
}

// SharingService orchestrates datapack sharing operations: invites,
// accept/decline, outgoing share views, and grant updates.
type SharingService struct {
	repo      *repository.SharingRepo
	cognito   CognitoInviter
	langfuseS *LangfuseService
}

// NewSharingService creates a new SharingService. The cognito parameter
// may be nil (e.g. in dev/test mode), in which case invite emails for
// non-existing users are skipped.
func NewSharingService(repo *repository.SharingRepo, cognito CognitoInviter) *SharingService {
	return &SharingService{repo: repo, cognito: cognito}
}

// SetCognito late-binds the CognitoInviter (used when the Cognito
// client is created after the SharingService).
func (s *SharingService) SetCognito(c CognitoInviter) {
	s.cognito = c
}

// SetLangfuse late-binds the LangfuseService used to sync
// datapack-share membership into Langfuse org/project memberships.
func (s *SharingService) SetLangfuse(l *LangfuseService) {
	s.langfuseS = l
}

// InviteRequest is the body for POST /datapacks/:name/share.
type InviteRequest struct {
	RecipientEmail string `json:"recipient_email"`
	GrantRole      string `json:"grant"`
}

// InviteResponse is returned when a new invite is created.
type InviteResponse struct {
	Status         string `json:"status"`
	InviteID       string `json:"invite_id"`
	RecipientEmail string `json:"recipient_email"`
	GrantRole      string `json:"grant"`
}

// InviteToDatapack creates a pending invite for the given datapack.
// If the invitee is not an existing meko user and a CognitoInviter is
// configured, a Cognito signup invitation email is sent.
func (s *SharingService) InviteToDatapack(
	ctx context.Context,
	datapackID, ownerUserID string,
	req InviteRequest,
) (*InviteResponse, error) {
	email := strings.TrimSpace(req.RecipientEmail)
	if email == "" {
		return nil, fmt.Errorf("recipient_email is required")
	}
	req.GrantRole = strings.ToLower(strings.TrimSpace(req.GrantRole))
	if !models.ValidGrantRole(req.GrantRole) {
		return nil, fmt.Errorf("grant is required and must be one of: viewer, contributor, maintainer")
	}

	recipientUserID, err := s.repo.LookupUserByEmail(email)
	if err != nil {
		return nil, fmt.Errorf("failed to look up recipient: %w", err)
	}

	inv, err := s.repo.CreateInvite(repository.CreateInviteParams{
		DatapackID:      datapackID,
		OwnerUserID:     ownerUserID,
		RecipientEmail:  email,
		RecipientUserID: recipientUserID,
		GrantRole:       req.GrantRole,
	})
	if err != nil {
		return nil, err
	}

	if recipientUserID == nil && s.cognito != nil {
		if cogErr := s.cognito.InviteUser(ctx, email, false); cogErr != nil {
			slog.Warn("failed to send cognito signup invite (non-fatal)",
				"email", email, "error", cogErr,
			)
		}
	}

	slog.Info("invite created",
		"invite_id", inv.InviteID,
		"datapack_id", datapackID,
		"recipient", email,
		"user_exists", recipientUserID != nil,
	)

	return &InviteResponse{
		Status:         inv.Status,
		InviteID:       inv.InviteID,
		RecipientEmail: inv.RecipientEmail,
		GrantRole:      inv.GrantRole,
	}, nil
}

// ListIncomingShares returns all pending invites for the given user's email.
func (s *SharingService) ListIncomingShares(email string) ([]models.IncomingShare, error) {
	shares, err := s.repo.GetPendingInvitesByEmail(email)
	if err != nil {
		return nil, err
	}
	if shares == nil {
		shares = []models.IncomingShare{}
	}
	return shares, nil
}

// InviteActionRequest is the body for POST /datapacks/invite-action.
type InviteActionRequest struct {
	InviteID        string `json:"invite_id"`
	RecipientUserID string `json:"recipient_user_id"`
	UserAction      string `json:"user_action"`
}

// InviteActionResponse is returned after acting on an invite.
type InviteActionResponse struct {
	Status   string `json:"status"`
	InviteID string `json:"invite_id"`
}

// HandleInviteAction processes an accept or decline action on a pending invite.
func (s *SharingService) HandleInviteAction(req InviteActionRequest) (*InviteActionResponse, error) {
	if req.InviteID == "" {
		return nil, fmt.Errorf("invite_id is required")
	}
	if req.RecipientUserID == "" {
		return nil, fmt.Errorf("recipient_user_id is required")
	}
	action := strings.ToLower(strings.TrimSpace(req.UserAction))
	if action != "accept" && action != "decline" {
		return nil, fmt.Errorf("user_action is required and must be 'accept' or 'decline'")
	}

	inv, err := s.repo.GetInviteByID(req.InviteID)
	if err != nil {
		return nil, err
	}
	if inv == nil || inv.Status != "pending" {
		return nil, fmt.Errorf("invite not found or not in pending status")
	}

	if inv.RecipientUserID == nil || *inv.RecipientUserID != req.RecipientUserID {
		recipientID, lookupErr := s.repo.LookupUserByEmail(inv.RecipientEmail)
		if lookupErr != nil {
			return nil, fmt.Errorf("failed to verify recipient: %w", lookupErr)
		}
		if recipientID == nil || *recipientID != req.RecipientUserID {
			return nil, fmt.Errorf("recipient_user_id does not match the invite")
		}
	}

	if action == "accept" {
		perms := grantToPermissions(inv.GrantRole)
		if err := s.repo.AcceptInvite(
			req.InviteID, req.RecipientUserID,
			inv.DatapackID, perms,
		); err != nil {
			return nil, err
		}
		if s.langfuseS != nil {
			firstName := ""
			lastName := ""
			if identity, iErr := s.repo.GetUserIdentityForShareSync(req.RecipientUserID); iErr != nil {
				slog.Warn("langfuse share sync: recipient identity lookup failed (continuing with email only)",
					"invite_id", req.InviteID,
					"recipient_user_id", req.RecipientUserID,
					"error", iErr)
			} else if identity != nil {
				firstName = identity.FirstName
				lastName = identity.LastName
			}
			if syncErr := s.langfuseS.SyncSharedDatapackMembership(
				inv.OwnerUserID,
				req.RecipientUserID,
				inv.RecipientEmail,
				firstName,
				lastName,
				inv.DatapackID,
				inv.GrantRole,
			); syncErr != nil {
				slog.Warn("langfuse share sync failed on invite accept (non-fatal)",
					"invite_id", req.InviteID,
					"datapack_id", inv.DatapackID,
					"owner_user_id", inv.OwnerUserID,
					"recipient_user_id", req.RecipientUserID,
					"error", syncErr)
			}
		}
		return &InviteActionResponse{Status: "accepted", InviteID: req.InviteID}, nil
	}

	if err := s.repo.DeclineInvite(req.InviteID); err != nil {
		return nil, err
	}
	return &InviteActionResponse{Status: "declined", InviteID: req.InviteID}, nil
}

// ListOutgoingPendingInvites returns pending (not yet accepted) invites for a datapack.
func (s *SharingService) ListOutgoingPendingInvites(datapackID string) (*models.OutgoingPendingInvitesResponse, error) {
	pending, err := s.repo.GetOutgoingPendingInvites(datapackID)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		pending = []models.OutgoingSharePendingInvite{}
	}
	return &models.OutgoingPendingInvitesResponse{PendingInvites: pending}, nil
}

// ListDatapackMembers returns accepted members (including synthetic owner row).
func (s *SharingService) ListDatapackMembers(
	datapackID, ownerUserID string,
) (*models.DatapackMembersResponse, error) {
	members, err := s.repo.GetOutgoingMembers(datapackID, ownerUserID)
	if err != nil {
		return nil, err
	}
	if members == nil {
		members = []models.OutgoingShareMember{}
	}
	return &models.DatapackMembersResponse{Members: members}, nil
}

// ResendInvite re-sends a pending invite. For non-existing users the
// Cognito signup email is resent; for existing users this is a no-op
// (they discover invites via GET /incoming-shares).
func (s *SharingService) ResendInvite(
	ctx context.Context,
	datapackID, ownerUserID, inviteID string,
) (string, error) {
	inv, err := s.repo.GetInviteByID(inviteID)
	if err != nil {
		return "", err
	}
	if inv == nil || inv.Status != "pending" || inv.DatapackID != datapackID || inv.OwnerUserID != ownerUserID {
		return "", fmt.Errorf("invite not found, not pending, or not owned by caller")
	}

	if inv.RecipientUserID == nil && s.cognito != nil {
		if cogErr := s.cognito.InviteUser(ctx, inv.RecipientEmail, true); cogErr != nil {
			slog.Warn("failed to resend cognito invite (non-fatal)",
				"email", inv.RecipientEmail, "error", cogErr,
			)
		}
	}

	return inv.InviteID, nil
}

// RevokeInvite cancels a pending invite.
func (s *SharingService) RevokeInvite(
	datapackID, ownerUserID, inviteID string,
) error {
	inv, err := s.repo.GetInviteByID(inviteID)
	if err != nil {
		return err
	}
	if inv == nil || inv.Status != "pending" || inv.DatapackID != datapackID || inv.OwnerUserID != ownerUserID {
		return fmt.Errorf("invite not found, not pending, or not owned by caller")
	}
	return s.repo.RevokeInvite(inviteID)
}

// UpdateGrantRequest is the body for PATCH /datapacks/:name/share/:share_user_id.
type UpdateGrantRequest struct {
	GrantRole string `json:"grant"`
}

// UpdateGrantResponse is returned after updating a member's grant.
type UpdateGrantResponse struct {
	UserID    string `json:"user_id"`
	GrantRole string `json:"grant"`
	Status    string `json:"status"`
}

// UpdateMemberGrant changes the access level of an active member.
func (s *SharingService) UpdateMemberGrant(
	datapackID, ownerUserID, memberUserID string,
	req UpdateGrantRequest,
) (*UpdateGrantResponse, error) {
	req.GrantRole = strings.ToLower(strings.TrimSpace(req.GrantRole))
	if !models.ValidGrantRole(req.GrantRole) {
		return nil, fmt.Errorf("grant is required and must be one of: viewer, contributor, maintainer")
	}

	perms := grantToPermissions(req.GrantRole)
	if err := s.repo.UpdateMemberGrant(datapackID, memberUserID, req.GrantRole, perms); err != nil {
		return nil, err
	}
	if s.langfuseS != nil {
		identity, iErr := s.repo.GetUserIdentityForShareSync(memberUserID)
		if iErr != nil {
			slog.Warn("langfuse share sync skipped: failed to load member identity",
				"datapack_id", datapackID, "member_user_id", memberUserID, "error", iErr)
		} else if identity != nil {
			if syncErr := s.langfuseS.SyncSharedDatapackMembership(
				ownerUserID,
				memberUserID,
				identity.Email,
				identity.FirstName,
				identity.LastName,
				datapackID,
				req.GrantRole,
			); syncErr != nil {
				slog.Warn("langfuse share sync failed on grant update (non-fatal)",
					"datapack_id", datapackID,
					"owner_user_id", ownerUserID,
					"member_user_id", memberUserID,
					"grant", req.GrantRole,
					"error", syncErr)
			}
		}
	}

	return &UpdateGrantResponse{
		UserID:    memberUserID,
		GrantRole: req.GrantRole,
		Status:    "updated",
	}, nil
}

// grantToPermissions maps an API grant role to the two
// agent_access_permissions rows (object_type, permission).
func grantToPermissions(grant string) []repository.ObjectPermission {
	switch models.GrantRole(grant) {
	case models.GrantViewer:
		return []repository.ObjectPermission{
			{ObjectType: "knowledge_base", Permission: "read"},
			{ObjectType: "memory", Permission: "read"},
		}
	case models.GrantContributor:
		return []repository.ObjectPermission{
			{ObjectType: "knowledge_base", Permission: "read"},
			{ObjectType: "memory", Permission: "write"},
		}
	case models.GrantMaintainer:
		return []repository.ObjectPermission{
			{ObjectType: "knowledge_base", Permission: "admin"},
			{ObjectType: "memory", Permission: "admin"},
		}
	default:
		return []repository.ObjectPermission{
			{ObjectType: "knowledge_base", Permission: "read"},
			{ObjectType: "memory", Permission: "read"},
		}
	}
}
