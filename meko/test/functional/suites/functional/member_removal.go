package functional

import (
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/member_removal",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "An owner, a viewer and a contributor on one datapack. The viewer cannot remove anyone (403), the owner cannot be " +
			"removed (400) and an unknown member is 404. Removing the contributor takes its REST access away at once (404 on the datapack, " +
			"gone from its list and the member list) and its MCP access within the access cache's bound. With link sharing on, the share link " +
			"refuses it (403 REMOVED_FROM_DATAPACK) while someone whose invite was only cancelled can still join; a new invite brings it back.",
		Run: memberRemoval,
	})
}

func memberRemoval(t *core.T) error {
	accts, done := accountsFor(t, 4)
	defer done()
	var us [4]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, viewer, member, cancelled := us[0], us[1], us[2], us[3]

	dp, err := ownedDatapack(t, owner, "dp-remove-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, mekoapi.GrantViewer); err != nil {
		return err
	}
	if err := shareWith(t, owner, member, dp.ID, mekoapi.GrantContributor); err != nil {
		return err
	}
	memberPath := func(id string) string { return "/datapacks/" + dp.ID + "/members/" + id }

	if err := t.Step("only_owners_and_maintainers_remove_and_never_the_owner", func(*core.StepHandle) error {
		if err := refused(t, "a viewer removing a member", viewer.api, http.MethodDelete, memberPath(member.acct.ID), nil, statusForbidden, "FORBIDDEN"); err != nil {
			return err
		}
		if err := refused(t, "removing the owner", owner.api, http.MethodDelete, memberPath(owner.acct.ID), nil, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
			return err
		}
		if err := refused(t, "removing someone who is not a member", owner.api, http.MethodDelete,
			memberPath("00000000-0000-0000-0000-000000000000"), nil, statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		g, err := member.api.GetDatapack(t, dp.ID)
		if err != nil || g.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("after the refused removals the member reads grant %q (%v), want contributor", g.Grant, err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("removal_takes_rest_access_away", func(*core.StepHandle) error {
		// An MCP call before the removal, so the MCP server has cached the member's
		// access and the next step proves the cached grant expires.
		if text, terr := member.call("conversation_list", map[string]any{"datapack_id": dp.ID}); terr != nil {
			return fmt.Errorf("the member's MCP call before removal failed: %s (%s)", terr.code, truncate(text, 300))
		}
		if err := owner.api.RemoveMember(t, dp.ID, member.acct.ID); err != nil {
			return err
		}
		if err := refused(t, "the removed member reading the datapack", member.api, http.MethodGet, "/datapacks/"+dp.ID, nil, statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		list, err := member.api.ListDatapacks(t)
		if err != nil {
			return err
		}
		if d := find(list, dp.ID); d != nil {
			return fmt.Errorf("the removed member's list still shows the datapack: %+v", d)
		}
		members, err := owner.api.Members(t, dp.ID)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.UserID == member.acct.ID {
				return fmt.Errorf("the member list still has the removed member: %+v", m)
			}
		}
		return refused(t, "removing the same member twice", owner.api, http.MethodDelete, memberPath(member.acct.ID), nil, statusNotFound, "NOT_FOUND")
	}); err != nil {
		return err
	}

	if err := t.Step("removal_takes_mcp_access_away", func(*core.StepHandle) error {
		// MCP caches a datapack access check for 60s, so the bound is that plus slack.
		var last string
		err := eventually(t, "the removed member's MCP access to end", 90*time.Second, func() (bool, error) {
			text, terr := member.call("conversation_create", map[string]any{"datapack_id": dp.ID, "title": "after removal"})
			if terr == nil {
				last = text
				return false, nil
			}
			last = terr.text
			return terr.code == "datapack_access_denied", nil
		})
		if err != nil {
			return fmt.Errorf("%w; last answer: %s", err, truncate(last, 300))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("the_share_link_cannot_bring_them_back", func(h *core.StepHandle) error {
		r, err := owner.api.Raw(t, http.MethodGet, "/datapacks/"+dp.ID+"/share-link", nil)
		if err != nil {
			return err
		}
		if r.Status == http.StatusNotFound {
			h.Set("skipped", "MEKO_ENABLE_LINK_SHARING is off on this stack")
			return nil
		}
		link, err := owner.api.UpdateShareLink(t, dp.ID, mekoapi.GrantViewer, mekoapi.ShareLinkActive)
		if err != nil {
			return err
		}
		if err := refused(t, "the removed member redeeming the share link", member.api, http.MethodPost,
			"/share/"+link.Token+"/redeem", nil, statusForbidden, "REMOVED_FROM_DATAPACK"); err != nil {
			return err
		}
		// A cancelled invite isn't a removal: that person can still join by link.
		inv, err := owner.api.ShareDatapack(t, dp.ID, cancelled.acct.Email, mekoapi.GrantViewer)
		if err != nil {
			return err
		}
		if err := owner.api.RevokeInvite(t, dp.ID, inv.InviteID); err != nil {
			return err
		}
		res, err := cancelled.api.RedeemShareLink(t, link.Token)
		if err != nil {
			return fmt.Errorf("someone whose invite was cancelled redeeming the link: %w", err)
		}
		if res.DatapackID != dp.ID || res.AlreadyMember {
			return fmt.Errorf("the cancelled invitee's redeem answered %+v, want a new member of %s", res, dp.ID)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("a_new_invite_brings_it_back", func(*core.StepHandle) error {
		if err := shareWith(t, owner, member, dp.ID, mekoapi.GrantViewer); err != nil {
			return err
		}
		g, err := member.api.GetDatapack(t, dp.ID)
		if err != nil || g.Grant != mekoapi.GrantViewer {
			return fmt.Errorf("after a new invite the member reads grant %q (%v), want viewer", g.Grant, err)
		}
		return nil
	})
}
