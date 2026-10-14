package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/telemetry"
)

// ErrCannotInviteOwner is returned by InviteToDatapack when the recipient
// email matches the datapack owner's email. The owner already has full
// access, so the invite is rejected regardless of who is inviting (the
// owner themselves or a maintainer). Handlers map this to 400.
var ErrCannotInviteOwner = errors.New("the owner already has full access to this datapack")

// ErrCannotRemoveOwner: the owner isn't a member who can be removed.
var ErrCannotRemoveOwner = errors.New("the datapack owner can't be removed")

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
	telemetry telemetry.Client
	// agentKAdminEmail: invites addressed to this one account are
	// auto-accepted (see InviteToDatapack) instead of left pending --
	// it's a designated service account (the shared AgentK deployment's
	// own identity), not a human who can click "accept" on every lab
	// user's share. Empty disables the behavior entirely.
	agentKAdminEmail string
}

// NewSharingService creates a new SharingService. The cognito parameter
// may be nil (e.g. in dev/test mode), in which case invite emails for
// non-existing users are skipped.
func NewSharingService(repo *repository.SharingRepo, cognito CognitoInviter) *SharingService {
	return &SharingService{repo: repo, cognito: cognito, telemetry: telemetry.NewNoop()}
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

// SetAgentKAdminEmail configures the one recipient address whose invites
// InviteToDatapack auto-accepts. Empty (the default) disables the behavior.
func (s *SharingService) SetAgentKAdminEmail(email string) {
	s.agentKAdminEmail = strings.TrimSpace(email)
}

// SetEmailer late-binds the EmailSender used to dispatch SES invite emails.
func (s *SharingService) SetEmailer(e EmailSender) {
	s.emailer = e
}

// SetTelemetry late-binds the Rudderstack emitter used for datapack_shared.
// The constructor seeds a Noop, so leaving this unset silently drops events.
func (s *SharingService) SetTelemetry(t telemetry.Client) {
	if t != nil {
		s.telemetry = t
	}
}

// InviteRequest is the body for POST /datapacks/:datapack_id/share.
type InviteRequest struct {
	RecipientEmail string `json:"recipient_email"`
	GrantRole      string `json:"grant"`
	// Populated by the handler from the auth context; never comes off the
	// JSON body. Feeds the datapack_shared Rudderstack event and does not
	// affect invite creation. ActingUserID is the caller's own user_id
	// (owner or maintainer) — the datapack_shared event MUST be attributed
	// to them, not to the datapack's owner (whose id we already carry
	// separately as ownerUserID for repo bookkeeping).
	ActingUserID  string `json:"-"`
	SharedByEmail string `json:"-"`
	SharedByRole  string `json:"-"`
	Source        string `json:"-"`
}

// InviteResponse is returned when a new invite is created.
type InviteResponse struct {
	Status         string `json:"status"`
	InviteID       string `json:"invite_id"`
	RecipientEmail string `json:"recipient_email"`
	GrantRole      string `json:"grant"`
	// IsNewUser is true when the recipient had no Meko account and one was
	// provisioned in Cognito as part of this invite. omitempty keeps the
	// single-invite response shape unchanged for existing users.
	IsNewUser bool `json:"is_new_user,omitempty"`
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

	// Block inviting the datapack owner: they already have full access, so
	// an invite to their email just creates a confusing pending invite /
	// redundant grant. Compare case-insensitively.
	if strings.EqualFold(email, strings.TrimSpace(ownerEmail)) {
		return nil, ErrCannotInviteOwner
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

	// A designated service account (the shared AgentK deployment's own
	// identity) can't click "accept" on every lab user's share -- auto-accept
	// on its behalf instead of leaving the invite pending. Scoped to exactly
	// one configured email; every other invite behaves exactly as before.
	if s.agentKAdminEmail != "" && recipientUserID != nil && strings.EqualFold(email, s.agentKAdminEmail) {
		if acceptErr := s.repo.AcceptInvite(inv.InviteID, *recipientUserID, datapackID, grantToPermissions(inv.GrantRole)); acceptErr != nil {
			slog.Warn("invite: agentk-admin auto-accept failed, invite left pending",
				"invite_id", inv.InviteID, "error", acceptErr)
		} else {
			inv.Status = "accepted"
		}
	}

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

	// Rudderstack datapack_shared. Closes the MCP / PAT / direct-API
	// funnel gap — those callers can't fire trackEvent from a browser,
	// so BE is the only place they land. UI shares are deliberately
	// skipped: meko_ui's InviteByEmailSection still fires this event
	// client-side, so a BE emit would double-count every browser share.
	// Disjoint on `source` (UI has none, BE carries mcp/api/admin), so
	// Growth can sum both paths without dedup logic.
	//
	// Attribution is the ACTING user (owner or maintainer who sent the
	// invite), not the datapack owner — attributing to the owner would
	// credit a maintainer's action to a different HubSpot identity and
	// break funnel joins with UI-side identify(). ActingUserID falls
	// back to ownerUserID only when the handler did not set it (bare-
	// bones callers, tests).
	source := req.Source
	if source == "" {
		source = telemetry.SourceAPI
	}
	if source != telemetry.SourceUI {
		actor := req.ActingUserID
		if actor == "" {
			actor = ownerUserID
		}
		total, countErr := s.repo.CountSharesByDatapack(datapackID)
		if countErr != nil {
			slog.Warn("telemetry: share count for datapack_shared failed",
				"datapack_id", datapackID, "error", countErr)
		}
		s.telemetry.Track(ctx, actor, telemetry.EventDatapackShared, map[string]any{
			"datapack_id":      datapackID,
			"shared_by_email":  req.SharedByEmail,
			"invited_email":    email,
			"shared_by_role":   req.SharedByRole,
			"invited_role":     req.GrantRole,
			"is_first_share":   total == 1,
			"total_shares":     total,
			"shared_timestamp": inv.CreatedAt,
			"source":           source,
		})
	}

	return &InviteResponse{
		Status:         inv.Status,
		InviteID:       inv.InviteID,
		RecipientEmail: inv.RecipientEmail,
		GrantRole:      inv.GrantRole,
		IsNewUser:      recipientUserID == nil,
	}, nil
}

// Per-row statuses returned by BulkInviteToDatapack. One of these lands in
// every BulkInviteResult.Status, and the same strings key the summary map.
const (
	// BulkStatusInvited: a pending invite was created for an existing account.
	BulkStatusInvited = "invited"
	// BulkStatusInvitedNewUser: invite created and a Cognito account was
	// provisioned because the recipient had no Meko account yet.
	BulkStatusInvitedNewUser = "invited_new_user"
	// BulkStatusAlreadyMember: the recipient already has access.
	BulkStatusAlreadyMember = "already_member"
	// BulkStatusInviteExists: a pending invite for this email already exists.
	BulkStatusInviteExists = "invite_exists"
	// BulkStatusIsOwner: the email is the datapack owner's — already full access.
	BulkStatusIsOwner = "is_owner"
	// BulkStatusInvalidEmail: the address failed format validation and was skipped.
	BulkStatusInvalidEmail = "invalid_email"
	// BulkStatusError: an unexpected failure; details are logged, not returned.
	BulkStatusError = "error"
)

// BulkInviteRequest is the body for POST /datapacks/:datapack_id/bulk-share.
// GrantRole applies to every email in the batch.
type BulkInviteRequest struct {
	Emails    []string `json:"emails"`
	GrantRole string   `json:"grant"`
	// Telemetry fields populated by the handler from the auth context,
	// never from the JSON body. Propagated verbatim into each per-row
	// InviteRequest the loop below builds, so datapack_shared events
	// (fired from InviteToDatapack) are attributed to the acting
	// maintainer and tagged with the correct `source` — a UI bulk-
	// share must land as source=ui so the service's skip gate fires and
	// the browser-side event isn't double-counted; a PAT/MCP bulk-share
	// must flow through at source=mcp/api so the funnel captures it.
	// Before this field set, every bulk-share landed as source=api and
	// credit went to the datapack owner.
	ActingUserID  string `json:"-"`
	SharedByEmail string `json:"-"`
	SharedByRole  string `json:"-"`
	Source        string `json:"-"`
}

// BulkInviteResult is the outcome for a single email in a bulk invite.
type BulkInviteResult struct {
	Email    string `json:"email"`
	Status   string `json:"status"`
	InviteID string `json:"invite_id,omitempty"`
	Message  string `json:"message,omitempty"`
}

// BulkInviteResponse aggregates per-email results plus a count per status.
type BulkInviteResponse struct {
	Results []BulkInviteResult `json:"results"`
	Summary map[string]int     `json:"summary"`
}

// BulkInviteToDatapack invites many emails to one datapack in a single call.
// Each row is processed independently (partial success); only a bad shared
// grant fails the whole request.
//
// Owner-collision is classified before the format guard so it matches
// POST /share even when the owner address would fail isValidEmailFormat
// (dotless domains). Remaining malformed rows are classified invalid_email
// without calling InviteToDatapack; every other case loops the single-invite
// path.
//
// Emails are de-duplicated case-insensitively; the first spelling of a repeated
// address is the one reported.
func (s *SharingService) BulkInviteToDatapack(
	ctx context.Context,
	datapackID, ownerUserID, datapackName, datapackDesc, ownerName, ownerEmail string,
	req BulkInviteRequest,
) (*BulkInviteResponse, error) {
	// The grant applies to the whole batch, so a bad one fails the request
	// rather than producing N identical per-row errors.
	req.GrantRole = strings.ToLower(strings.TrimSpace(req.GrantRole))
	if !models.ValidGrantRole(req.GrantRole) {
		return nil, fmt.Errorf("grant is required and must be one of: viewer, contributor, maintainer")
	}
	if len(req.Emails) == 0 {
		return nil, fmt.Errorf("emails is required and must contain at least one address")
	}

	resp := &BulkInviteResponse{
		Results: make([]BulkInviteResult, 0, len(req.Emails)),
		Summary: map[string]int{},
	}
	seen := make(map[string]struct{}, len(req.Emails))

	for _, raw := range req.Emails {
		// Client disconnect or upstream timeout (meko-ui nginx
		// proxy_read_timeout, 180 s) cancels ctx. Bail rather than
		// keep provisioning Cognito users and firing SES sends for
		// rows the caller no longer cares about; the batch-cap comment
		// on handlers/sharing_handler.go assumes the request is alive
		// for the whole ~35 s worst-case, and cancellation is the
		// other half of that budget.
		if ctxErr := ctx.Err(); ctxErr != nil {
			slog.Info("bulk invite: aborting after cancellation",
				"datapack_id", datapackID,
				"processed", len(resp.Results),
				"remaining", len(req.Emails)-len(resp.Results),
				"error", ctxErr,
			)
			return resp, ctxErr
		}

		email := strings.TrimSpace(raw)
		key := strings.ToLower(email)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		// Owner-collision before the format guard: isValidEmailFormat rejects
		// dotless domains, so checking format first would mis-label an owner
		// address like the seeded `dev@localhost`.
		if owner := strings.TrimSpace(ownerEmail); owner != "" && strings.EqualFold(email, owner) {
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusIsOwner,
				Message: ErrCannotInviteOwner.Error(),
			})
			continue
		}

		if !isValidEmailFormat(email) {
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusInvalidEmail,
				Message: "not a valid email address",
			})
			continue
		}

		inv, err := s.InviteToDatapack(
			ctx, datapackID, ownerUserID, datapackName, datapackDesc, ownerName, ownerEmail,
			InviteRequest{
				RecipientEmail: email,
				GrantRole:      req.GrantRole,
				// Carry the auth-context fields through so each per-row
				// datapack_shared event picks up actor attribution and
				// the right `source` tag; without this the batch would
				// double-count browser shares (both UI and BE firing)
				// and credit maintainer actions to the datapack owner.
				ActingUserID:  req.ActingUserID,
				SharedByEmail: req.SharedByEmail,
				SharedByRole:  req.SharedByRole,
				Source:        req.Source,
			},
		)
		switch {
		case err == nil:
			status := BulkStatusInvited
			if inv.IsNewUser {
				status = BulkStatusInvitedNewUser
			}
			resp.add(BulkInviteResult{Email: email, Status: status, InviteID: inv.InviteID})
		case errors.Is(err, repository.ErrAlreadyMember):
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusAlreadyMember,
				Message: "already has access to this datapack",
			})
		case errors.Is(err, repository.ErrInviteExists):
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusInviteExists,
				Message: "a pending invite already exists for this email",
			})
		case errors.Is(err, ErrCannotInviteOwner):
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusIsOwner,
				Message: ErrCannotInviteOwner.Error(),
			})
		default:
			// This path can carry raw Cognito/AWS SDK errors (IAM ARNs,
			// request IDs) — log them, return a generic per-row message.
			slog.Error("bulk invite: row failed",
				"datapack_id", datapackID, "recipient", email, "error", err)
			resp.add(BulkInviteResult{
				Email:   email,
				Status:  BulkStatusError,
				Message: "failed to send invite, please try again or contact support",
			})
		}
	}

	slog.Info("bulk invite complete",
		"datapack_id", datapackID,
		"requested", len(req.Emails),
		"processed", len(resp.Results),
		"summary", resp.Summary,
	)
	return resp, nil
}

// add appends a result and keeps the status summary in sync.
func (r *BulkInviteResponse) add(res BulkInviteResult) {
	r.Results = append(r.Results, res)
	r.Summary[res.Status]++
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
	InviteID string `json:"invite_id"`
	// RecipientUserID is set by the handler from the authenticated account,
	// never from the request body -- a caller-supplied value would let
	// anyone holding an invite_id act on another person's invite. It stays
	// on the struct because HandleInviteAction authorizes against it.
	RecipientUserID string `json:"-"`
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

// UpdateGrantRequest is the body for PATCH /datapacks/:datapack_id/share/:share_user_id.
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
	s.SyncLangfuseMembership(ownerUserID, memberUserID, datapackID, req.GrantRole)

	return &UpdateGrantResponse{
		UserID:    memberUserID,
		GrantRole: req.GrantRole,
		Status:    "updated",
	}, nil
}

// SyncLangfuseMembership brings a member's Langfuse project membership in
// line with their grant. Best-effort: a failure is logged, never returned,
// because Langfuse being down must not undo a sharing change.
func (s *SharingService) SyncLangfuseMembership(ownerUserID, memberUserID, datapackID, grant string) {
	if s.langfuseS == nil {
		return
	}
	identity, err := s.repo.GetUserIdentityForShareSync(memberUserID)
	if err != nil || identity == nil {
		slog.Warn("langfuse share sync skipped: failed to load member identity",
			"datapack_id", datapackID, "member_user_id", memberUserID, "error", err)
		return
	}
	if err := s.langfuseS.SyncSharedDatapackMembership(
		ownerUserID, memberUserID, identity.Email, identity.FirstName, identity.LastName, datapackID, grant,
	); err != nil {
		slog.Warn("langfuse share sync failed (non-fatal)",
			"datapack_id", datapackID, "owner_user_id", ownerUserID,
			"member_user_id", memberUserID, "grant", grant, "error", err)
	}
}

// SyncLangfuseMembershipToCurrentGrant syncs a member against the role the
// database holds now, rather than one passed in that may be stale.
func (s *SharingService) SyncLangfuseMembershipToCurrentGrant(ownerUserID, memberUserID, datapackID string) {
	grant, err := s.repo.CurrentGrant(datapackID, memberUserID)
	if err != nil || grant == "" {
		slog.Warn("langfuse share sync skipped: no current grant",
			"datapack_id", datapackID, "member_user_id", memberUserID, "error", err)
		return
	}
	s.SyncLangfuseMembership(ownerUserID, memberUserID, datapackID, grant)
}

// RemoveMember takes an accepted member's access to the datapack away,
// including their Langfuse project membership.
func (s *SharingService) RemoveMember(datapackID, ownerUserID, memberUserID string) error {
	if memberUserID == ownerUserID {
		return ErrCannotRemoveOwner
	}
	email, err := s.repo.RemoveMember(datapackID, memberUserID)
	if err != nil {
		return err
	}
	// The Langfuse user is keyed on the member's current email; the invite
	// row keeps the address they were invited at, which may have changed.
	identity, iErr := s.repo.GetUserIdentityForShareSync(memberUserID)
	if iErr == nil && identity != nil && identity.Email != "" {
		email = identity.Email
	}
	if s.langfuseS != nil {
		if lfErr := s.langfuseS.RemoveSharedDatapackMembership(ownerUserID, memberUserID, email, datapackID); lfErr != nil {
			slog.Warn("langfuse membership removal failed (non-fatal)",
				"datapack_id", datapackID, "member_user_id", memberUserID, "error", lfErr)
		}
	}
	return nil
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
