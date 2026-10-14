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
	// Source is how an accepted member joined: "email" or "link". Empty on the owner row.
	Source string `json:"source"`
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

// RemoveMember takes an accepted member's access to the datapack away.
func (c *Client) RemoveMember(ctx context.Context, datapackID, memberUserID string) error {
	return c.do(ctx, http.MethodDelete, "/datapacks/"+url.PathEscape(datapackID)+"/members/"+url.PathEscape(memberUserID), nil, nil)
}

// Share link statuses.
const (
	ShareLinkActive = "active"
	ShareLinkOff    = "off"
)

// ShareLink is a datapack's one share link.
type ShareLink struct {
	DatapackID string `json:"datapack_id"`
	Token      string `json:"token"`
	Grant      string `json:"grant"`
	Status     string `json:"status"`
	// RedemptionCount is the members who joined through the link and are still members.
	RedemptionCount int `json:"redemption_count"`
	// MembersAtLinkRole counts link joiners still at the link's role.
	MembersAtLinkRole int `json:"members_at_link_role"`
}

type ShareLinkPreview struct {
	DatapackName string `json:"datapack_name"`
	Description  string `json:"description"`
}

type ShareLinkRedemption struct {
	DatapackID    string `json:"datapack_id"`
	Grant         string `json:"grant"`
	AlreadyMember bool   `json:"already_member"`
}

// GetShareLink returns the datapack's link, or nil when it has none.
func (c *Client) GetShareLink(ctx context.Context, datapackID string) (*ShareLink, error) {
	var out struct {
		ShareLink *ShareLink `json:"share_link"`
	}
	err := c.do(ctx, http.MethodGet, "/datapacks/"+url.PathEscape(datapackID)+"/share-link", nil, &out)
	return out.ShareLink, err
}

// UpdateShareLink creates the link or changes its grant or status; an empty
// argument keeps the current value.
func (c *Client) UpdateShareLink(ctx context.Context, datapackID, grant, status string) (ShareLink, error) {
	body := map[string]string{}
	if grant != "" {
		body["grant"] = grant
	}
	if status != "" {
		body["status"] = status
	}
	var out struct {
		ShareLink ShareLink `json:"share_link"`
	}
	err := c.do(ctx, http.MethodPut, "/datapacks/"+url.PathEscape(datapackID)+"/share-link", body, &out)
	return out.ShareLink, err
}

// ResetShareLink replaces the link's token; the old URL stops working.
func (c *Client) ResetShareLink(ctx context.Context, datapackID string) (ShareLink, error) {
	var out struct {
		ShareLink ShareLink `json:"share_link"`
	}
	err := c.do(ctx, http.MethodPut, "/datapacks/"+url.PathEscape(datapackID)+"/share-link",
		map[string]bool{"reset": true}, &out)
	return out.ShareLink, err
}

// PreviewShareLink is what a signed-in caller who hasn't joined sees: the
// datapack's name and description. Signed out, the route answers 401.
func (c *Client) PreviewShareLink(ctx context.Context, token string) (ShareLinkPreview, error) {
	var out ShareLinkPreview
	err := c.do(ctx, http.MethodGet, "/share/"+url.PathEscape(token), nil, &out)
	return out, err
}

// RedeemShareLink joins the caller to the datapack behind the link.
func (c *Client) RedeemShareLink(ctx context.Context, token string) (ShareLinkRedemption, error) {
	var out ShareLinkRedemption
	err := c.do(ctx, http.MethodPost, "/share/"+url.PathEscape(token)+"/redeem", nil, &out)
	return out, err
}
