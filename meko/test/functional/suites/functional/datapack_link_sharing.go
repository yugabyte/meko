package functional

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/datapack_link_sharing",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Seven accounts on a datapack's share link, with MEKO_ENABLE_LINK_SHARING on (skipped when the stack leaves it off). " +
			"A maintainer link and an unknown status are 400. A preview needs sign-in (401 signed out) and shows only the name and description. Two redeems at once " +
			"by a stranger make one viewer and count once; a third changes nothing; a viewer cannot read or change the link. A contributor " +
			"who redeems a viewer link stays contributor, and a pending contributor invite is accepted as contributor. Switching the link to " +
			"contributor moves its viewer joiners to contributor and admits new members as contributors; switching back moves them to " +
			"viewer, except one whose role the owner changed by hand. An email-invited viewer who redeems the contributor link is raised and then moves with it; " +
			"other email-invited members never move. Someone who declined an invite joins through the link as a new member. Turned off, the preview and a redeem are 404 and " +
			"members keep access, and its role can't change until it is on again; turned back on, the same token works. Reset gives a new token: the old URL is 404 and members keep access. " +
			"Deleting the datapack kills the link.",
		Run: datapackLinkSharing,
	})
}

func datapackLinkSharing(t *core.T) error {
	accts, done := accountsFor(t, 7)
	defer done()
	owner, contributor, joiner, invitee, late := accts[0], accts[1], accts[2], accts[3], accts[4]
	emailViewer, decliner := accts[5], accts[6]
	name := "dp-link-" + t.ResourceSuffix()

	var dp mekoapi.Datapack
	deleted := false
	defer func() {
		if dp.ID == "" || deleted {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = owner.API.DeleteDatapack(ctx, dp.ID)
	}()
	var err error
	if dp, err = createDatapack(t, owner.API, name); err != nil {
		return err
	}

	linkPath := "/datapacks/" + dp.ID + "/share-link"
	if err := t.Step("link_sharing_is_on", func(*core.StepHandle) error {
		r, err := owner.API.Raw(t, http.MethodGet, linkPath, nil)
		if err != nil {
			return err
		}
		// With the flag on the owner always gets 200 (no link yet reads null),
		// so a 404 means the route is not registered.
		if r.Status == http.StatusNotFound {
			t.Skipf("the share-link routes are absent: MEKO_ENABLE_LINK_SHARING is off on this stack")
		}
		return wantStatus("the owner reading a datapack with no link yet", r, http.StatusOK, "")
	}); err != nil {
		return err
	}

	inv, err := owner.API.ShareDatapack(t, dp.ID, contributor.Email, mekoapi.GrantContributor)
	if err != nil {
		return err
	}
	if _, err := contributor.API.ActOnInvite(t, inv.InviteID, "accept"); err != nil {
		return err
	}
	pending, err := owner.API.ShareDatapack(t, dp.ID, invitee.Email, mekoapi.GrantContributor)
	if err != nil {
		return err
	}
	viewerInv, err := owner.API.ShareDatapack(t, dp.ID, emailViewer.Email, mekoapi.GrantViewer)
	if err != nil {
		return err
	}
	if _, err := emailViewer.API.ActOnInvite(t, viewerInv.InviteID, "accept"); err != nil {
		return err
	}
	declined, err := owner.API.ShareDatapack(t, dp.ID, decliner.Email, mekoapi.GrantContributor)
	if err != nil {
		return err
	}
	if _, err := decliner.API.ActOnInvite(t, declined.InviteID, "decline"); err != nil {
		return err
	}

	var link mekoapi.ShareLink
	if err := t.Step("owner_creates_a_viewer_link", func(h *core.StepHandle) error {
		for _, bad := range []map[string]string{{"grant": mekoapi.GrantMaintainer}, {"grant": "owner"}, {"status": "paused"}} {
			if err := refused(t, fmt.Sprintf("a link with %v", bad), owner.API, http.MethodPut, linkPath, bad, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
				return err
			}
		}
		if got, err := owner.API.GetShareLink(t, dp.ID); err != nil || got != nil {
			return fmt.Errorf("after the refused requests the datapack has link %+v (%v), want none", got, err)
		}
		if link, err = owner.API.UpdateShareLink(t, dp.ID, "", ""); err != nil {
			return err
		}
		h.Set("redemption_count", link.RedemptionCount)
		if link.Token == "" || link.Grant != mekoapi.GrantViewer || link.Status != mekoapi.ShareLinkActive || link.RedemptionCount != 0 {
			return fmt.Errorf("a new link reads %+v, want an active viewer link with a token and no joins", link)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("preview_needs_sign_in_and_shows_only_name_and_description", func(*core.StepHandle) error {
		path := "/share/" + link.Token
		if err := refused(t, "a signed-out preview", anonymousAPI(t), http.MethodGet, path, nil, statusUnauth, "", name); err != nil {
			return err
		}
		// late hasn't joined yet: a signed-in stranger is who previews a link.
		r, err := late.API.Raw(t, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if err := wantStatus("a signed-in preview", r, http.StatusOK, ""); err != nil {
			return err
		}
		for _, leak := range []string{owner.Email, owner.ID, dp.ID} {
			if strings.Contains(string(r.Body), leak) {
				return fmt.Errorf("the preview names %q: %s", leak, truncateBody(r.Body))
			}
		}
		p, err := late.API.PreviewShareLink(t, link.Token)
		if err != nil {
			return err
		}
		if p.DatapackName != name {
			return fmt.Errorf("the preview names %q, want %q", p.DatapackName, name)
		}
		return refused(t, "the preview of an unknown token", late.API, http.MethodGet, "/share/no-such-token-"+t.ResourceSuffix(), nil, statusNotFound, "NOT_FOUND")
	}); err != nil {
		return err
	}

	count := func() (int, error) {
		l, err := owner.API.GetShareLink(t, dp.ID)
		if err != nil || l == nil {
			return 0, fmt.Errorf("the owner reading the link: %+v, %v", l, err)
		}
		return l.RedemptionCount, nil
	}
	grantOf := func(a *mekoapi.Client) (string, error) {
		d, err := a.GetDatapack(t, dp.ID)
		return d.Grant, err
	}

	var errs []error
	if err := t.Step("two_redeems_at_once_make_one_member", func(h *core.StepHandle) error {
		var wg sync.WaitGroup
		res := make([]mekoapi.ShareLinkRedemption, 2)
		rerr := make([]error, 2)
		for i := range res {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res[i], rerr[i] = joiner.API.RedeemShareLink(t, link.Token)
			}(i)
		}
		wg.Wait()
		joined := 0
		for i := range res {
			if rerr[i] != nil {
				return fmt.Errorf("redeem %d: %w", i, rerr[i])
			}
			if res[i].DatapackID != dp.ID || res[i].Grant != mekoapi.GrantViewer {
				return fmt.Errorf("redeem %d answered %+v, want %s as viewer", i, res[i], dp.ID)
			}
			if !res[i].AlreadyMember {
				joined++
			}
		}
		n, err := count()
		if err != nil {
			return err
		}
		h.Set("redemption_count", n)
		if joined != 1 || n != 1 {
			return fmt.Errorf("two concurrent redeems joined %d times and the link counts %d, want one join counted once", joined, n)
		}
		g, err := grantOf(joiner.API)
		if err != nil || g != mekoapi.GrantViewer {
			return fmt.Errorf("the joiner reads the datapack with grant %q (%v), want viewer", g, err)
		}
		members, err := owner.API.Members(t, dp.ID)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.UserEmail == joiner.Email {
				if m.Source != "link" || m.Grant != mekoapi.GrantViewer {
					return fmt.Errorf("the member list has the joiner as %+v, want a viewer with source link", m)
				}
				return nil
			}
		}
		return fmt.Errorf("the owner's member list %+v lacks the joiner %s", members, joiner.Email)
	}); err != nil {
		return err
	}

	if err := t.Step("redeeming_again_changes_nothing", func(*core.StepHandle) error {
		r, err := joiner.API.RedeemShareLink(t, link.Token)
		if err != nil {
			return err
		}
		n, err := count()
		if err != nil {
			return err
		}
		if !r.AlreadyMember || r.Grant != mekoapi.GrantViewer || n != 1 {
			return fmt.Errorf("a repeat redeem answered %+v and the link counts %d, want already_member as viewer and still 1", r, n)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("a_viewer_cannot_manage_the_link", func(*core.StepHandle) error {
		if err := refused(t, "a viewer reading the link", joiner.API, http.MethodGet, linkPath, nil, statusForbidden, "FORBIDDEN", link.Token); err != nil {
			return err
		}
		return refused(t, "a viewer turning the link off", joiner.API, http.MethodPut, linkPath,
			map[string]string{"status": mekoapi.ShareLinkOff}, statusForbidden, "FORBIDDEN")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("a_link_never_lowers_a_role", func(*core.StepHandle) error {
		r, err := contributor.API.RedeemShareLink(t, link.Token)
		if err != nil {
			return err
		}
		if !r.AlreadyMember || r.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("the contributor's redeem of a viewer link answered %+v, want already_member as contributor", r)
		}
		if g, err := grantOf(contributor.API); err != nil || g != mekoapi.GrantContributor {
			return fmt.Errorf("after redeeming, the contributor reads grant %q (%v), want contributor", g, err)
		}
		if n, err := count(); err != nil || n != 1 {
			return fmt.Errorf("an existing member's redeem moved the count to %d (%v), want 1", n, err)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("a_pending_invite_is_accepted_at_the_higher_role", func(*core.StepHandle) error {
		r, err := invitee.API.RedeemShareLink(t, link.Token)
		if err != nil {
			return err
		}
		if r.AlreadyMember || r.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("the invitee's redeem answered %+v, want a new member as contributor (the invite's role)", r)
		}
		incoming, err := invitee.API.IncomingShares(t)
		if err != nil {
			return err
		}
		for _, s := range incoming {
			if s.InviteID == pending.InviteID {
				return fmt.Errorf("the invite %s is still pending after the invitee joined through the link", pending.InviteID)
			}
		}
		// The count is the link's current members, and the invite kept its own role
		// and its email source, so the invitee isn't one of them.
		if n, err := count(); err != nil || n != 1 {
			return fmt.Errorf("the link counts %d (%v) after the invitee joined on their invite, want still 1", n, err)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("a_role_change_moves_the_link_members", func(h *core.StepHandle) error {
		up, err := owner.API.UpdateShareLink(t, dp.ID, mekoapi.GrantContributor, "")
		if err != nil {
			return err
		}
		if up.Token != link.Token || up.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("switching the role answered %+v, want the same token %s as contributor", up, link.Token)
		}
		if g, err := grantOf(joiner.API); err != nil || g != mekoapi.GrantContributor {
			return fmt.Errorf("after the switch the viewer who joined by link reads grant %q (%v), want contributor", g, err)
		}
		r, err := late.API.RedeemShareLink(t, link.Token)
		if err != nil {
			return err
		}
		if r.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("a join after the switch answered %+v, want contributor", r)
		}
		l, err := owner.API.GetShareLink(t, dp.ID)
		if err != nil || l == nil || l.MembersAtLinkRole != 2 {
			return fmt.Errorf("the link reports %+v (%v), want 2 members at its role (the two link joiners)", l, err)
		}
		h.Set("members_at_link_role", l.MembersAtLinkRole)

		// An email-invited viewer redeeming the contributor link is raised and
		// becomes a link member, so the switch back below moves them too.
		if r, err = emailViewer.API.RedeemShareLink(t, link.Token); err != nil {
			return err
		}
		if !r.AlreadyMember || r.Grant != mekoapi.GrantContributor {
			return fmt.Errorf("the email-invited viewer's redeem of the contributor link answered %+v, want already_member raised to contributor", r)
		}

		// The owner sets the late joiner by hand to the link's own role, so only
		// the hand-set mark (not a different role) keeps it from moving back.
		if err := owner.API.UpdateGrant(t, dp.ID, late.ID, mekoapi.GrantContributor); err != nil {
			return err
		}
		if _, err := owner.API.UpdateShareLink(t, dp.ID, mekoapi.GrantViewer, ""); err != nil {
			return err
		}
		for _, want := range []struct {
			who   string
			api   *mekoapi.Client
			grant string
		}{
			{"the link joiner", joiner.API, mekoapi.GrantViewer},
			{"the email-invited viewer the link raised", emailViewer.API, mekoapi.GrantViewer},
			{"the joiner whose role the owner set by hand", late.API, mekoapi.GrantContributor},
			{"the email-invited contributor", contributor.API, mekoapi.GrantContributor},
			{"the invitee whose invite the link accepted", invitee.API, mekoapi.GrantContributor},
		} {
			if g, err := grantOf(want.api); err != nil || g != want.grant {
				return fmt.Errorf("after switching back to viewer %s reads grant %q (%v), want %s", want.who, g, err, want.grant)
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("a_declined_invite_joins_as_new", func(*core.StepHandle) error {
		r, err := decliner.API.RedeemShareLink(t, link.Token)
		if err != nil {
			return err
		}
		if r.AlreadyMember || r.Grant != mekoapi.GrantViewer {
			return fmt.Errorf("the decliner's redeem of the viewer link answered %+v, want a new member as viewer, not the declined invite's contributor", r)
		}
		members, err := owner.API.Members(t, dp.ID)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.UserEmail == decliner.Email {
				if m.Source != "link" || m.Grant != mekoapi.GrantViewer {
					return fmt.Errorf("the member list has the decliner as %+v, want a viewer with source link", m)
				}
				return nil
			}
		}
		return fmt.Errorf("the decliner isn't in the member list after joining through the link")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("off_stops_joins_and_keeps_members", func(*core.StepHandle) error {
		l, err := owner.API.UpdateShareLink(t, dp.ID, "", mekoapi.ShareLinkOff)
		if err != nil {
			return err
		}
		if l.Status != mekoapi.ShareLinkOff || l.Token != link.Token {
			return fmt.Errorf("turning the link off answered %+v", l)
		}
		if err := refused(t, "the preview of a link that is off", late.API, http.MethodGet, "/share/"+link.Token, nil, statusNotFound, "NOT_FOUND", name); err != nil {
			return err
		}
		if err := refused(t, "a redeem of a link that is off", joiner.API, http.MethodPost, "/share/"+link.Token+"/redeem", nil, statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		if g, err := grantOf(joiner.API); err != nil || g != mekoapi.GrantViewer {
			return fmt.Errorf("with the link off the joiner reads grant %q (%v), want viewer still", g, err)
		}
		// A role change while off would move every past joiner unseen.
		if err := refused(t, "a role change while the link is off", owner.API, http.MethodPut, "/datapacks/"+dp.ID+"/share-link",
			map[string]string{"grant": mekoapi.GrantContributor}, http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
			return err
		}
		on, err := owner.API.UpdateShareLink(t, dp.ID, "", mekoapi.ShareLinkActive)
		if err != nil {
			return err
		}
		if on.Token != link.Token {
			return fmt.Errorf("turning the link back on gave token %s, want the same %s", on.Token, link.Token)
		}
		_, err = late.API.PreviewShareLink(t, link.Token)
		return err
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("reset_kills_the_old_url", func(h *core.StepHandle) error {
		old := link.Token
		fresh, err := owner.API.ResetShareLink(t, dp.ID)
		if err != nil {
			return err
		}
		if fresh.Token == "" || fresh.Token == old || fresh.Status != mekoapi.ShareLinkActive {
			return fmt.Errorf("a reset answered %+v, want an active link with a new token", fresh)
		}
		link = fresh
		h.Set("token_changed", true)
		if err := refused(t, "the preview of a reset link's old URL", late.API, http.MethodGet, "/share/"+old, nil,
			statusNotFound, "NOT_FOUND", name); err != nil {
			return err
		}
		if err := refused(t, "a redeem of a reset link's old URL", late.API, http.MethodPost, "/share/"+old+"/redeem", nil,
			statusNotFound, "NOT_FOUND"); err != nil {
			return err
		}
		if _, err := late.API.PreviewShareLink(t, fresh.Token); err != nil {
			return fmt.Errorf("the new URL after a reset: %w", err)
		}
		if g, err := grantOf(joiner.API); err != nil || g != mekoapi.GrantViewer {
			return fmt.Errorf("after a reset the joiner reads grant %q (%v), want viewer still", g, err)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("deleting_the_datapack_kills_the_link", func(*core.StepHandle) error {
		if err := owner.API.DeleteDatapack(t, dp.ID); err != nil {
			return err
		}
		deleted = true
		return eventually(t, "the deleted datapack's link to answer 404", visibleWithin(t), func() (bool, error) {
			r, err := late.API.Raw(t, http.MethodGet, "/share/"+link.Token, nil)
			if err != nil {
				return false, err
			}
			return r.Status == http.StatusNotFound, nil
		})
	}); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
