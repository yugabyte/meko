package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// CognitoInviter abstracts Cognito account provisioning for sharing invites.
type CognitoInviter interface {
	// CreateUser provisions a new Cognito account with the given temporary
	// password, suppressing Cognito's own email. Returns ("", nil) if the
	// user already exists so callers can treat it as a no-op.
	CreateUser(ctx context.Context, email, temporaryPassword string) (cognitoSub string, err error)
	// ResetTempPassword sets a new temporary password for an existing
	// unconfirmed Cognito user. Used when resending an invite.
	ResetTempPassword(ctx context.Context, email, temporaryPassword string) error
}

// InviteEmailParams carries the context needed to render an invitation email.
type InviteEmailParams struct {
	RecipientEmail string
	DatapackName   string
	DatapackDesc   string
	OwnerName      string
	OwnerEmail     string
	GrantRole      string
	AppURL         string
	TempPassword   string // non-empty only for new (non-existing) users
	IsExistingUser bool
}

// EmailSender abstracts SES email dispatch. The service package defines the
// interface; only router.go imports both service and email to wire them.
type EmailSender interface {
	SendInviteEmail(ctx context.Context, p InviteEmailParams) error
}

// genTempPassword generates a cryptographically random 16-character password
// that satisfies Cognito's default complexity policy: at least one uppercase,
// one lowercase, one digit, and one symbol character.
func genTempPassword() (string, error) {
	const (
		upper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		lower   = "abcdefghijklmnopqrstuvwxyz"
		digits  = "0123456789"
		symbols = "!@#$%^&*"
		all     = upper + lower + digits + symbols
		length  = 16
	)

	pickFrom := func(charset string) (byte, error) {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return 0, err
		}
		return charset[idx.Int64()], nil
	}

	buf := make([]byte, length)
	var err error
	// Guarantee at least one character from each required class.
	for i, cs := range []string{upper, lower, digits, symbols} {
		if buf[i], err = pickFrom(cs); err != nil {
			return "", fmt.Errorf("genTempPassword: %w", err)
		}
	}
	for i := 4; i < length; i++ {
		if buf[i], err = pickFrom(all); err != nil {
			return "", fmt.Errorf("genTempPassword: %w", err)
		}
	}
	// Fisher-Yates shuffle so mandatory chars don't always appear at 0-3.
	for i := length - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", fmt.Errorf("genTempPassword shuffle: %w", err)
		}
		j := int(jBig.Int64())
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf), nil
}

// SharingService orchestrates datapack sharing operations: invites,
// accept/decline, outgoing share views, and grant updates.
type SharingService struct {
	repo      *repository.SharingRepo
	cognito   CognitoInviter
	langfuseS *LangfuseService
	emailer   EmailSender
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

// SetEmailer late-binds the EmailSender used to dispatch SES invite emails.
func (s *SharingService) SetEmailer(e EmailSender) {
	s.emailer = e
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
// For new users, a Cognito account is provisioned (email suppressed) and an
// SES invite email with a temporary password is sent. For existing users, an
// SES notification email is sent. Both email sends are non-fatal.
func (s *SharingService) InviteToDatapack(
	ctx context.Context,
	datapackID, ownerUserID, datapackName, datapackDesc, ownerName, ownerEmail string,
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

	// Pre-check: reject duplicates BEFORE the insert. Catches deployments
	// that lack the partial-unique-index plus the already-accepted case
	// (where CreateInvite would otherwise happily add a fresh pending row
	// alongside an active member).
	// Normalise to lower-case so the pre-check matches regardless of how
	// the caller (or an existing pending invite) capitalised the email.
	// The repo also `lower()`s the column on its side for symmetry.
	switch existing, lookupErr := s.repo.LookupInviteStatus(datapackID, strings.ToLower(email)); {
	case lookupErr != nil:
		return nil, lookupErr
	case existing == "pending":
		return nil, fmt.Errorf("%w: email=%s datapack=%s", repository.ErrInviteExists, email, datapackID)
	case existing == "accepted":
		return nil, fmt.Errorf("%w: email=%s datapack=%s", repository.ErrAlreadyMember, email, datapackID)
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

	slog.Info("invite created",
		"invite_id", inv.InviteID,
		"datapack_id", datapackID,
		"recipient", email,
		"user_exists", recipientUserID != nil,
	)

	emailParams := InviteEmailParams{
		RecipientEmail: email,
		DatapackName:   datapackName,
		DatapackDesc:   datapackDesc,
		OwnerName:      ownerName,
		OwnerEmail:     ownerEmail,
		GrantRole:      req.GrantRole,
		IsExistingUser: recipientUserID != nil,
	}

	if recipientUserID == nil {
		// New user: provision Cognito account (suppress Cognito's own email),
		// then send an SES welcome email with the temporary password.
		if s.cognito == nil {
			return nil, fmt.Errorf("invite: cognito not configured — cannot provision account for new user")
		}
		tempPwd, pwdErr := genTempPassword()
		if pwdErr != nil {
			return nil, fmt.Errorf("invite: failed to generate temp password: %w", pwdErr)
		}
		if _, cogErr := s.cognito.CreateUser(ctx, email, tempPwd); cogErr != nil {
			return nil, fmt.Errorf("invite: cognito create user failed: %w", cogErr)
		}
		emailParams.TempPassword = tempPwd
	}

	if s.emailer != nil {
		if mailErr := s.emailer.SendInviteEmail(ctx, emailParams); mailErr != nil {
			slog.Warn("invite: ses email failed (non-fatal)",
				"invite_id", inv.InviteID, "email", email, "error", mailErr)
		}
	} else {
		slog.Debug("invite: email sender not configured, skipping invite email",
			"invite_id", inv.InviteID, "email", email)
	}

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

// ResendInvite re-sends a pending invite. For new users a fresh temporary
// password is set in Cognito and an SES email is sent with the new password.
// For existing users an SES reminder notification is sent.
func (s *SharingService) ResendInvite(
	ctx context.Context,
	datapackID, ownerUserID, datapackName, datapackDesc, ownerName, ownerEmail, inviteID string,
) (string, error) {
	inv, err := s.repo.GetInviteByID(inviteID)
	if err != nil {
		return "", err
	}
	if inv == nil || inv.Status != "pending" || inv.DatapackID != datapackID || inv.OwnerUserID != ownerUserID {
		return "", fmt.Errorf("invite not found, not pending, or not owned by caller")
	}

	slog.Info("resend invite",
		"invite_id", inviteID,
		"datapack_id", datapackID,
		"recipient", inv.RecipientEmail,
		"user_exists", inv.RecipientUserID != nil,
	)

	emailParams := InviteEmailParams{
		RecipientEmail: inv.RecipientEmail,
		DatapackName:   datapackName,
		DatapackDesc:   datapackDesc,
		OwnerName:      ownerName,
		OwnerEmail:     ownerEmail,
		GrantRole:      inv.GrantRole,
		IsExistingUser: inv.RecipientUserID != nil,
	}

	if inv.RecipientUserID == nil {
		// New user: reset the Cognito temp password so the fresh SES email
		// contains a valid credential.
		if s.cognito == nil {
			return "", fmt.Errorf("resend: cognito not configured — cannot reset temp password for new user")
		}
		tempPwd, pwdErr := genTempPassword()
		if pwdErr != nil {
			return "", fmt.Errorf("resend: failed to generate temp password: %w", pwdErr)
		}
		if cogErr := s.cognito.ResetTempPassword(ctx, inv.RecipientEmail, tempPwd); cogErr != nil {
			return "", fmt.Errorf("resend: cognito reset temp password failed: %w", cogErr)
		}
		emailParams.TempPassword = tempPwd
	}

	if s.emailer != nil {
		if mailErr := s.emailer.SendInviteEmail(ctx, emailParams); mailErr != nil {
			slog.Warn("resend: ses email failed (non-fatal)",
				"invite_id", inviteID, "email", inv.RecipientEmail, "error", mailErr)
		}
	} else {
		slog.Debug("resend: email sender not configured, skipping invite email",
			"invite_id", inviteID, "email", inv.RecipientEmail)
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
