package service

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ErrInvalidShareLink wraps a request the link can't accept: a role other
// than viewer or contributor, or an unknown status. Handlers map it to 400.
var ErrInvalidShareLink = errors.New("invalid share link request")

// ShareLinkService manages the one share link a datapack can have, and lets a
// signed-in user join a datapack through it.
type ShareLinkService struct {
	repo    *repository.ShareLinkRepo
	sharing *SharingService
	// syncLocks serializes Langfuse syncs per datapack, so two quick role
	// changes can't land out of order. A fixed pool keyed by a hash of the
	// datapack id, so it doesn't grow with every datapack ever synced; two
	// datapacks sharing a lock only wait on each other.
	syncLocks [64]sync.Mutex
}

func NewShareLinkService(repo *repository.ShareLinkRepo, sharing *SharingService) *ShareLinkService {
	return &ShareLinkService{repo: repo, sharing: sharing}
}

// UpdateShareLinkRequest is the body for PUT /datapacks/:datapack_id/share-link.
// Omitted fields keep their current value; a new link defaults to an active
// viewer link.
type UpdateShareLinkRequest struct {
	GrantRole *string `json:"grant,omitempty"`
	Status    *string `json:"status,omitempty"`
	// Reset replaces the link's token, so a leaked URL stops working; members
	// who already joined keep their access.
	Reset bool `json:"reset,omitempty"`
}

// Get returns the datapack's link, or nil when it has none yet.
func (s *ShareLinkService) Get(datapackID string) (*models.DatapackShareLink, error) {
	return s.repo.Get(datapackID)
}

// Update creates the datapack's link or changes its role or status.
func (s *ShareLinkService) Update(
	datapackID, callerUserID string, req UpdateShareLinkRequest,
) (*models.DatapackShareLink, error) {
	// Only the fields the request sets are sent; the repo fills the rest from
	// the locked row, so concurrent updates of different fields both stick.
	var grant, status string
	if req.GrantRole != nil {
		grant = strings.ToLower(strings.TrimSpace(*req.GrantRole))
		if !models.ValidShareLinkGrant(grant) {
			return nil, fmt.Errorf("%w: grant must be viewer or contributor", ErrInvalidShareLink)
		}
	}
	if req.Status != nil {
		status = strings.ToLower(strings.TrimSpace(*req.Status))
		if status != models.ShareLinkStatusActive && status != models.ShareLinkStatusOff {
			return nil, fmt.Errorf("%w: status must be active or off", ErrInvalidShareLink)
		}
	}
	token, err := newShareLinkToken()
	if err != nil {
		return nil, err
	}
	res, err := s.repo.Upsert(datapackID, token, grant, status, callerUserID, req.Reset, grantToPermissions)
	if errors.Is(err, repository.ErrShareLinkRace) {
		res, err = s.repo.Upsert(datapackID, token, grant, status, callerUserID, req.Reset, grantToPermissions)
	}
	if errors.Is(err, repository.ErrShareLinkOffRoleChange) {
		return nil, fmt.Errorf("%w: turn the link on before changing its role", ErrInvalidShareLink)
	}
	if err != nil {
		return nil, err
	}
	if len(res.Moved) > 0 && s.sharing != nil {
		go s.syncMoved(res.OwnerUserID, datapackID, res.Moved)
	}
	return res.Link, nil
}

// syncMoved brings Langfuse in line with the moved members' roles, off the
// request because a link can have many members. One datapack's syncs run one
// at a time, and each reads the member's role at sync time, so the last role
// change wins in Langfuse as it did in the database.
func (s *ShareLinkService) syncMoved(ownerUserID, datapackID string, moved []string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(datapackID))
	mu := &s.syncLocks[h.Sum32()%uint32(len(s.syncLocks))]
	mu.Lock()
	defer mu.Unlock()
	for _, uid := range moved {
		s.sharing.SyncLangfuseMembershipToCurrentGrant(ownerUserID, uid, datapackID)
	}
}

// Preview returns what a visitor to the link sees before joining.
func (s *ShareLinkService) Preview(token string) (*models.ShareLinkPreview, error) {
	return s.repo.Preview(strings.TrimSpace(token))
}

// Redeem joins the account to the datapack behind the link.
func (s *ShareLinkService) Redeem(token string, account *models.Account) (*models.ShareLinkRedemption, error) {
	res, err := s.repo.Redeem(strings.TrimSpace(token), account.ID, account.Email, decideRedemption, grantToPermissions)
	if err != nil {
		return nil, err
	}
	if (res.NewMember || res.Raised) && s.sharing != nil {
		s.sharing.SyncLangfuseMembership(res.OwnerUserID, account.ID, res.DatapackID, res.Grant)
	}
	return res, nil
}

// decideRedemption is the role a redeem leaves the caller with, from their
// existing invite and the link's role.
func decideRedemption(ex repository.ExistingInvite, linkGrant string) repository.RedeemDecision {
	switch ex.Status {
	case "accepted":
		// A role the owner set by hand stays, whatever the link offers.
		if ex.Source == models.InviteSourceLinkOverride {
			return repository.RedeemDecision{Grant: ex.Grant, Source: ex.Source}
		}
		// A link can only raise a member's role: a maintainer who clicks a viewer link stays maintainer.
		if models.GrantRank(linkGrant) <= models.GrantRank(ex.Grant) {
			return repository.RedeemDecision{Grant: ex.Grant, Source: ex.Source}
		}
		return repository.RedeemDecision{Grant: linkGrant, Source: models.InviteSourceLink, Change: true}
	case "pending":
		// A pending invite at a higher role than the link's keeps the
		// invite's role, and so stays an email member.
		if models.GrantRank(ex.Grant) > models.GrantRank(linkGrant) {
			return repository.RedeemDecision{Grant: ex.Grant, Source: models.InviteSourceEmail}
		}
	}
	return repository.RedeemDecision{Grant: linkGrant, Source: models.InviteSourceLink}
}

// newShareLinkToken returns 128 random bits, URL-safe.
func newShareLinkToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate share link token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
