package mekoapi

import (
	"context"
	"net/http"
	"net/url"
)

// Grant roles a datapack can be shared with. The owner's own grant reads "owner".
const (
	GrantOwner       = "owner"
	GrantMaintainer  = "maintainer"
	GrantContributor = "contributor"
	GrantViewer      = "viewer"
)

type Invite struct {
	Status         string `json:"status"`
	InviteID       string `json:"invite_id"`
	RecipientEmail string `json:"recipient_email"`
	Grant          string `json:"grant"`
	IsNewUser      bool   `json:"is_new_user"`
}

type IncomingShare struct {
	InviteID     string `json:"invite_id"`
	OwnerUserID  string `json:"owner_user_id"`
	OwnerName    string `json:"owner_name"`
	OwnerEmail   string `json:"owner_email"`
	Grant        string `json:"grant"`
	DatapackID   string `json:"datapack_id"`
	DatapackName string `json:"datapack_name"`
}

type PendingInvite struct {
	InviteID         string  `json:"invite_id"`
	Grant            string  `json:"grant"`
	RecipientUserID  *string `json:"recipient_user_id"`
	RecipientEmailID string  `json:"recipient_email_id"`
}

type Member struct {
	UserID    string `json:"user_id"`
	IsOwner   bool   `json:"is_owner"`
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email_id"`
	Grant     string `json:"grant"`
}

type InviteAction struct {
	Status   string `json:"status"`
	InviteID string `json:"invite_id"`
}

func (c *Client) ShareDatapack(ctx context.Context, datapackID, recipientEmail, grant string) (Invite, error) {
	var out Invite
	err := c.do(ctx, http.MethodPost, "/datapacks/"+url.PathEscape(datapackID)+"/share",
		map[string]string{"recipient_email": recipientEmail, "grant": grant}, &out)
	return out, err
}

func (c *Client) IncomingShares(ctx context.Context) ([]IncomingShare, error) {
	var out []IncomingShare
	err := c.do(ctx, http.MethodGet, "/datapacks/incoming-shares", nil, &out)
	return out, err
}

// ActOnInvite accepts or declines an invite addressed to the caller. action is
// "accept" or "decline".
func (c *Client) ActOnInvite(ctx context.Context, inviteID, action string) (InviteAction, error) {
	var out InviteAction
	err := c.do(ctx, http.MethodPost, "/datapacks/invite-action",
		map[string]string{"invite_id": inviteID, "user_action": action}, &out)
	return out, err
}

func (c *Client) OutgoingShares(ctx context.Context, datapackID string) ([]PendingInvite, error) {
	var out struct {
		PendingInvites []PendingInvite `json:"pending_invites"`
	}
	err := c.do(ctx, http.MethodGet, "/datapacks/"+url.PathEscape(datapackID)+"/outgoing-shares", nil, &out)
	return out.PendingInvites, err
}

func (c *Client) Members(ctx context.Context, datapackID string) ([]Member, error) {
	var out struct {
		Members []Member `json:"members"`
	}
	err := c.do(ctx, http.MethodGet, "/datapacks/"+url.PathEscape(datapackID)+"/member-list", nil, &out)
	return out.Members, err
}

func (c *Client) RevokeInvite(ctx context.Context, datapackID, inviteID string) error {
	return c.do(ctx, http.MethodDelete, "/datapacks/"+url.PathEscape(datapackID)+"/invite/"+url.PathEscape(inviteID), nil, nil)
}

// UpdateGrant changes an accepted member's grant. It is JWT only.
func (c *Client) UpdateGrant(ctx context.Context, datapackID, memberUserID, grant string) error {
	return c.do(ctx, http.MethodPatch, "/datapacks/"+url.PathEscape(datapackID)+"/share/"+url.PathEscape(memberUserID),
		map[string]string{"grant": grant}, nil)
}
