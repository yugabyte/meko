package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/datapack_sharing",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Three accounts on a stack with the Cognito mock. A creates a datapack and shares it with B as a viewer. The invite is pending, B sees it as an incoming share and cannot read the datapack yet. " +
			"B accepts, reads the datapack with the viewer grant, is on A's member list but cannot read it (403), and lists it over MCP with its own PAT. B cannot share it on (403). C, who was never invited, gets 404 on REST and does not see it over MCP. " +
			"A raises B to contributor. A second invite for the same email is a 409, an invite for the owner a 400, and an invite for another user cannot be accepted by C (403). " +
			"A pending invite that is revoked cannot be accepted (404) and grants nothing. Removing an accepted member is not covered here.",
		Run: datapackSharing,
	})
}

func datapackSharing(t *core.T) error {
	accts, cleanup := accountsFor(t, 3)
	defer cleanup()
	a, b, c := accts[0], accts[1], accts[2]
	name := "dp-share-" + t.ResourceSuffix()

	var dp mekoapi.Datapack
	defer func() {
		if dp.ID == "" {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = a.API.DeleteDatapack(ctx, dp.ID)
	}()
	if err := t.Step("owner_creates_a_datapack", func(h *core.StepHandle) error {
		var err error
		dp, err = a.API.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name})
		if err != nil {
			return err
		}
		h.Set("datapack_id", dp.ID)
		got, err := a.API.GetDatapack(t, dp.ID)
		if err != nil {
			return err
		}
		if got.Grant != mekoapi.GrantOwner {
			return fmt.Errorf("the owner's grant reads %q, want owner", got.Grant)
		}
		return nil
	}); err != nil {
		return err
	}

	var invite mekoapi.Invite
	if err := t.Step("invite_is_pending_and_grants_nothing", func(h *core.StepHandle) error {
		var err error
		invite, err = a.API.ShareDatapack(t, dp.ID, b.Email, mekoapi.GrantViewer)
		if err != nil {
			return err
		}
		h.Set("invite_id", invite.InviteID)
		if invite.Status != "pending" || invite.InviteID == "" || invite.Grant != mekoapi.GrantViewer || invite.IsNewUser {
			return fmt.Errorf("invite answered %+v, want a pending viewer invite for an existing user", invite)
		}
		incoming, err := b.API.IncomingShares(t)
		if err != nil {
			return err
		}
		var seen *mekoapi.IncomingShare
		for i := range incoming {
			if incoming[i].InviteID == invite.InviteID {
				seen = &incoming[i]
			}
		}
		if seen == nil || seen.DatapackID != dp.ID || seen.Grant != mekoapi.GrantViewer || seen.OwnerEmail != a.Email {
			return fmt.Errorf("B's incoming shares %+v lack the invite for %s from %s", incoming, dp.ID, a.Email)
		}
		if err := refuses(t, "B reading the datapack before accepting", b.API, http.MethodGet, "/datapacks/"+dp.ID, statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		pending, err := a.API.OutgoingShares(t, dp.ID)
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].InviteID != invite.InviteID || pending[0].RecipientEmailID != b.Email {
			return fmt.Errorf("A's outgoing shares %+v, want the one invite to %s", pending, b.Email)
		}
		cIncoming, err := c.API.IncomingShares(t)
		if err != nil {
			return err
		}
		for _, s := range cIncoming {
			if s.DatapackID == dp.ID {
				return fmt.Errorf("C has an incoming share for a datapack nobody invited it to: %+v", s)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("only_the_invited_user_can_accept", func(*core.StepHandle) error {
		got, err := c.API.Raw(t, http.MethodPost, "/datapacks/invite-action", map[string]string{"invite_id": invite.InviteID, "user_action": "accept"})
		if err != nil {
			return err
		}
		if err := wantStatus("C accepting B's invite", got, http.StatusForbidden, "FORBIDDEN"); err != nil {
			return err
		}
		return refuses(t, "C reading the datapack after trying to accept B's invite", c.API, http.MethodGet, "/datapacks/"+dp.ID, statusNotFound, "NOT_FOUND")
	}); err != nil {
		return err
	}

	if err := t.Step("accept_gives_the_viewer_grant", func(*core.StepHandle) error {
		res, err := b.API.ActOnInvite(t, invite.InviteID, "accept")
		if err != nil {
			return err
		}
		if res.Status != "accepted" {
			return fmt.Errorf("accept answered %+v", res)
		}
		got, err := b.API.GetDatapack(t, dp.ID)
		if err != nil {
			return err
		}
		if got.ID != dp.ID || got.Name != name || got.Grant != mekoapi.GrantViewer {
			return fmt.Errorf("B reads %+v, want %s %q with the viewer grant", got, dp.ID, name)
		}
		list, err := b.API.ListDatapacks(t)
		if err != nil {
			return err
		}
		if d := find(list, dp.ID); d == nil || d.Grant != mekoapi.GrantViewer {
			return fmt.Errorf("B's list has %+v for the shared datapack, want it with the viewer grant", d)
		}
		members, err := a.API.Members(t, dp.ID)
		if err != nil {
			return err
		}
		var viewer bool
		for _, m := range members {
			viewer = viewer || (m.UserEmail == b.Email && m.Grant == mekoapi.GrantViewer && !m.IsOwner)
		}
		if !viewer {
			return fmt.Errorf("A's member list %+v lacks B as a viewer", members)
		}
		if err := refuses(t, "B reading the member list as a viewer", b.API, http.MethodGet,
			"/datapacks/"+dp.ID+"/member-list", statusForbidden, "FORBIDDEN"); err != nil {
			return err
		}
		again, err := b.API.Raw(t, http.MethodPost, "/datapacks/invite-action", map[string]string{"invite_id": invite.InviteID, "user_action": "accept"})
		if err != nil {
			return err
		}
		return wantStatus("accepting the same invite twice", again, http.StatusNotFound, "NOT_FOUND")
	}); err != nil {
		return err
	}

	if err := t.Step("viewer_cannot_share_on", func(*core.StepHandle) error {
		got, err := b.API.Raw(t, http.MethodPost, "/datapacks/"+dp.ID+"/share", map[string]string{"recipient_email": c.Email, "grant": "viewer"})
		if err != nil {
			return err
		}
		if err := wantStatus("a viewer sharing the datapack", got, http.StatusForbidden, "FORBIDDEN"); err != nil {
			return err
		}
		return refuses(t, "a viewer reading the pending invites", b.API, http.MethodGet, "/datapacks/"+dp.ID+"/outgoing-shares", statusForbidden, "FORBIDDEN")
	}); err != nil {
		return err
	}

	if err := t.Step("mcp_follows_the_grant", func(h *core.StepHandle) error {
		bView, err := mcpView(t, b.MCP(b.PAT().Value), dp.ID, name)
		if err != nil {
			return fmt.Errorf("B over MCP: %w", err)
		}
		if !bView.listed || !strings.Contains(bView.describe, dp.ID) {
			return fmt.Errorf("B over MCP: listed=%v, datapack_describe answered %s, want %q in both", bView.listed, bView.describe, name)
		}
		if !strings.Contains(bView.update, "NOT_FOUND") {
			return fmt.Errorf("B updating the datapack over MCP: %s, want NOT_FOUND, a viewer is not the owner", bView.update)
		}
		cView, err := mcpView(t, c.MCP(c.PAT().Value), dp.ID, name)
		if err != nil {
			return fmt.Errorf("C over MCP: %w", err)
		}
		if cView.listed || !strings.Contains(cView.describe, "datapack_access_denied") || !strings.Contains(cView.update, "NOT_FOUND") {
			return fmt.Errorf("C over MCP: listed=%v, describe %s, update %s, want it unlisted, access denied and NOT_FOUND", cView.listed, cView.describe, cView.update)
		}
		aView, err := mcpView(t, a.MCP(a.PAT().Value), dp.ID, name)
		if err != nil {
			return fmt.Errorf("A over MCP: %w", err)
		}
		if !aView.listed || !strings.Contains(aView.update, "datapack updated") {
			return fmt.Errorf("A over MCP: listed=%v, update %s, want the owner to list and update it", aView.listed, aView.update)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("stranger_is_refused_everywhere", func(*core.StepHandle) error {
		for _, route := range []string{
			"/datapacks/" + dp.ID, "/datapacks/" + dp.ID + "/member-list", "/datapacks/" + dp.ID + "/outgoing-shares",
			"/datapacks/" + dp.ID + "/provision-status",
		} {
			if err := refuses(t, "C on GET "+route, c.API, http.MethodGet, route, statusNotFound, "NOT_FOUND"); err != nil {
				return err
			}
		}
		if err := refuses(t, "C sharing the datapack", c.API, http.MethodPost, "/datapacks/"+dp.ID+"/share", statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		list, err := c.API.ListDatapacks(t)
		if err != nil {
			return err
		}
		if d := find(list, dp.ID); d != nil {
			return fmt.Errorf("C's list shows the datapack: %+v", d)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("grant_can_be_raised", func(*core.StepHandle) error {
		if err := a.API.UpdateGrant(t, dp.ID, b.ID, mekoapi.GrantContributor); err != nil {
			return err
		}
		got, err := b.API.GetDatapack(t, dp.ID)
		if err != nil {
			return err
		}
		if got.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("B's grant reads %q after the update, want contributor", got.Grant)
		}
		bad, err := a.API.Raw(t, http.MethodPatch, "/datapacks/"+dp.ID+"/share/"+b.ID, map[string]string{"grant": "root"})
		if err != nil {
			return err
		}
		return wantStatus("an unknown grant", bad, http.StatusBadRequest, "VALIDATION_ERROR")
	}); err != nil {
		return err
	}

	if err := t.Step("invite_rules", func(*core.StepHandle) error {
		dup, err := a.API.Raw(t, http.MethodPost, "/datapacks/"+dp.ID+"/share", map[string]string{"recipient_email": b.Email, "grant": "viewer"})
		if err != nil {
			return err
		}
		if err := wantStatus("inviting B again after B accepted", dup, http.StatusConflict, "ALREADY_MEMBER"); err != nil {
			return err
		}
		self, err := a.API.Raw(t, http.MethodPost, "/datapacks/"+dp.ID+"/share", map[string]string{"recipient_email": a.Email, "grant": "viewer"})
		if err != nil {
			return err
		}
		if err := wantStatus("inviting the owner", self, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
			return err
		}
		badGrant, err := a.API.Raw(t, http.MethodPost, "/datapacks/"+dp.ID+"/share", map[string]string{"recipient_email": c.Email, "grant": "root"})
		if err != nil {
			return err
		}
		return wantStatus("inviting with an unknown grant", badGrant, http.StatusBadRequest, "VALIDATION_ERROR")
	}); err != nil {
		return err
	}

	return t.Step("revoked_invite_cannot_be_accepted", func(*core.StepHandle) error {
		inv, err := a.API.ShareDatapack(t, dp.ID, c.Email, mekoapi.GrantViewer)
		if err != nil {
			return err
		}
		twice, err := a.API.Raw(t, http.MethodPost, "/datapacks/"+dp.ID+"/share", map[string]string{"recipient_email": c.Email, "grant": "viewer"})
		if err != nil {
			return err
		}
		if err := wantStatus("a second pending invite for the same email", twice, http.StatusConflict, "CONFLICT"); err != nil {
			return err
		}
		if err := a.API.RevokeInvite(t, dp.ID, inv.InviteID); err != nil {
			return err
		}
		got, err := c.API.Raw(t, http.MethodPost, "/datapacks/invite-action", map[string]string{"invite_id": inv.InviteID, "user_action": "accept"})
		if err != nil {
			return err
		}
		if err := wantStatus("accepting a revoked invite", got, http.StatusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		return refuses(t, "C reading the datapack after its invite was revoked", c.API, http.MethodGet, "/datapacks/"+dp.ID, statusNotFound, "NOT_FOUND")
	})
}

func find(list []mekoapi.Datapack, id string) *mekoapi.Datapack {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func refuses(ctx context.Context, what string, api *mekoapi.Client, method, path string, status int, code string) error {
	got, err := api.Raw(ctx, method, path, nil)
	if err != nil {
		return err
	}
	return wantStatus(what, got, status, code)
}

// mcpResult is what one account sees of a datapack over MCP. The MCP server
// answers a refused call as ordinary text with isError false, so the text is
// what a test reads.
type mcpResult struct {
	listed           bool
	describe, update string
}

func mcpView(ctx context.Context, c *mcpclient.Client, id, name string) (mcpResult, error) {
	defer func() {
		cctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		c.Close(cctx)
	}()
	var out mcpResult
	if _, err := c.Initialize(ctx); err != nil {
		return out, err
	}
	call := func(tool string, args map[string]any) (string, error) {
		res, err := c.CallTool(ctx, tool, args)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", fmt.Errorf("%s answered a tool error: %s", tool, res.Text())
		}
		return res.Text(), nil
	}
	conv, err := call("conversation_create", map[string]any{})
	if err != nil {
		return out, err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(conv), &created); err != nil || created.ID == "" {
		return out, fmt.Errorf("conversation_create answered %s", conv)
	}
	list, err := call("datapack_list", map[string]any{"conversation_id": created.ID})
	if err != nil {
		return out, err
	}
	out.listed = strings.Contains(list, name)
	if out.describe, err = call("datapack_describe", map[string]any{"datapack_id": id, "conversation_id": created.ID}); err != nil {
		return out, err
	}
	out.update, err = call("datapack_update", map[string]any{"datapack_id": id, "description": "set over MCP", "conversation_id": created.ID})
	return out, err
}
