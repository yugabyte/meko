package functional

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/access_matrix",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Four accounts on a stack with the Cognito mock: the owner of a datapack, a maintainer, a viewer and a stranger. Each calls the datapack routes. " +
			"Read routes answer every member and 404 the stranger. Routes for owners and maintainers answer 403 to a viewer. Owner-only routes answer 404 to everyone else, the maintainer included. " +
			"Then PAT scopes, which a stack with no identity provider cannot test because it ignores the Bearer header: a read PAT is refused on write routes with 403 INSUFFICIENT_SCOPE and a write PAT on read routes, " +
			"on member routes the scope check runs before the grant check, on owner-only routes the ownership check runs before the scope check, a PAT is 401 on routes that take JWTs only, and a revoked PAT is 401.",
		Run: accessMatrix,
	})
}

type role int

const (
	owner role = iota
	maintainer
	viewer
	stranger
)

var roleNames = [...]string{"owner", "maintainer", "viewer", "stranger"}

// route is one request and what each role is told, in owner, maintainer, viewer,
// stranger order. A zero status means the role is not asserted.
type route struct {
	method, path string
	body         any
	want         [4]int
}

var (
	anyMember     = [4]int{200, 200, 200, 404}
	ownerOrMaint  = [4]int{200, 200, 403, 404}
	ownerOnly     = [4]int{200, 404, 404, 404}
	notOwnerOnly  = [4]int{0, 404, 404, 404}
	pendingChange = map[string]bool{"opted_out": false}
)

func accessMatrix(t *core.T) error {
	accts, cleanup := accountsFor(t, 4)
	defer cleanup()
	a, m, v, s := accts[0], accts[1], accts[2], accts[3]
	by := [4]*accountRef{{a, "owner"}, {m, "maintainer"}, {v, "viewer"}, {s, "stranger"}}

	dp, err := a.API.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: "dp-matrix-" + t.ResourceSuffix()})
	if err != nil {
		return fmt.Errorf("create the datapack: %w", err)
	}
	deleted := false
	defer func() {
		if deleted {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = a.API.DeleteDatapack(ctx, dp.ID)
	}()

	if err := t.Step("share_with_a_maintainer_and_a_viewer", func(*core.StepHandle) error {
		for _, g := range []struct {
			who   *accountRef
			grant string
		}{{by[maintainer], mekoapi.GrantMaintainer}, {by[viewer], mekoapi.GrantViewer}} {
			inv, err := a.API.ShareDatapack(t, dp.ID, g.who.a.Email, g.grant)
			if err != nil {
				return err
			}
			if _, err := g.who.a.API.ActOnInvite(t, inv.InviteID, "accept"); err != nil {
				return err
			}
			got, err := g.who.a.API.GetDatapack(t, dp.ID)
			if err != nil || got.Grant != g.grant {
				return fmt.Errorf("%s reads grant %q (%v), want %s", g.who.name, got.Grant, err, g.grant)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	id := dp.ID
	if err := t.Step("routes_by_role", func(h *core.StepHandle) error {
		routes := []route{
			{http.MethodGet, "/datapacks/" + id, nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/member-list", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/provision-status", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/agents", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/artifacts", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/knowledge-bases", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/knowledge-bases/counts", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/knowledge-bases/summary", nil, anyMember},
			{http.MethodGet, "/datapacks/" + id + "/outgoing-shares", nil, ownerOrMaint},
			{http.MethodPatch, "/datapacks/" + id + "/conversation-search-opt-out", pendingChange, ownerOrMaint},
			{http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "matrix"}, ownerOnly},
			{http.MethodGet, "/datapacks/" + id + "/status", nil, ownerOnly},
			{http.MethodPost, "/datapacks/" + id + "/agents", map[string]string{"agent_id": "no-such-agent"}, notOwnerOnly},
			{http.MethodPost, "/datapacks/" + id + "/langfuse/project-keys", nil, notOwnerOnly},
			{http.MethodDelete, "/datapacks/" + id, nil, notOwnerOnly},
		}
		var errs []error
		for _, r := range routes {
			for i, who := range by {
				if r.want[i] == 0 {
					continue
				}
				got, err := who.a.API.Raw(t, r.method, r.path, r.body)
				if err != nil {
					return err
				}
				if got.Status != r.want[i] {
					errs = append(errs, fmt.Errorf("%s %s as %s: HTTP %d %q, want %d: %s",
						r.method, strings.TrimPrefix(r.path, "/datapacks/"+id), who.name, got.Status, got.Code, r.want[i], truncateBody(got.Body)))
				}
			}
		}
		h.Set("requests", len(routes)*4)
		return errors.Join(errs...)
	}); err != nil {
		return err
	}

	if err := t.Step("ids_that_are_not_datapacks", func(*core.StepHandle) error {
		for _, who := range by {
			if err := refuses(t, who.name+" on a malformed id", who.a.API, http.MethodGet, "/datapacks/not-a-uuid", http.StatusBadRequest, "VALIDATION_ERROR"); err != nil {
				return err
			}
			if err := refuses(t, who.name+" on an unknown id", who.a.API, http.MethodGet, "/datapacks/00000000-0000-4000-8000-000000000000", statusNotFound, "NOT_FOUND"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("pat_scopes", func(*core.StepHandle) error {
		read, write := a.PAT(mekoapi.ScopeRead), a.PAT(mekoapi.ScopeWrite)
		all := a.PAT()
		type pc struct {
			who    string
			client *mekoapi.Client
			method string
			path   string
			body   any
			status int
			code   string
		}
		scope := "INSUFFICIENT_SCOPE"
		vRead, vWrite := v.PAT(mekoapi.ScopeRead), v.PAT(mekoapi.ScopeWrite)
		sRead, sWrite := s.PAT(mekoapi.ScopeRead), s.PAT(mekoapi.ScopeWrite)
		cases := []pc{
			{"owner read PAT, list", a.WithPAT(read.Value), http.MethodGet, "/datapacks", nil, 200, ""},
			{"owner read PAT, describe", a.WithPAT(read.Value), http.MethodGet, "/datapacks/" + id, nil, 200, ""},
			{"owner read PAT, outgoing shares", a.WithPAT(read.Value), http.MethodGet, "/datapacks/" + id + "/outgoing-shares", nil, 200, ""},
			{"owner read PAT, create", a.WithPAT(read.Value), http.MethodPost, "/datapacks", map[string]string{"name": "x"}, 403, scope},
			{"owner read PAT, update", a.WithPAT(read.Value), http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "x"}, 403, scope},
			{"owner read PAT, delete", a.WithPAT(read.Value), http.MethodDelete, "/datapacks/" + id, nil, 403, scope},
			{"owner read PAT, share", a.WithPAT(read.Value), http.MethodPost, "/datapacks/" + id + "/share", map[string]string{"recipient_email": s.Email, "grant": "viewer"}, 403, scope},
			{"owner write PAT, list", a.WithPAT(write.Value), http.MethodGet, "/datapacks", nil, 403, scope},
			{"owner write PAT, describe", a.WithPAT(write.Value), http.MethodGet, "/datapacks/" + id, nil, 403, scope},
			{"owner write PAT, update", a.WithPAT(write.Value), http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "by write PAT"}, 200, ""},
			{"owner all-scope PAT, describe", a.WithPAT(all.Value), http.MethodGet, "/datapacks/" + id, nil, 200, ""},
			{"owner all-scope PAT, update", a.WithPAT(all.Value), http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "by all-scope PAT"}, 200, ""},
			{"viewer read PAT, describe", v.WithPAT(vRead.Value), http.MethodGet, "/datapacks/" + id, nil, 200, ""},
			{"viewer read PAT, share (scope first)", v.WithPAT(vRead.Value), http.MethodPost, "/datapacks/" + id + "/share", map[string]string{"recipient_email": s.Email, "grant": "viewer"}, 403, scope},
			{"viewer write PAT, share (then grant)", v.WithPAT(vWrite.Value), http.MethodPost, "/datapacks/" + id + "/share", map[string]string{"recipient_email": s.Email, "grant": "viewer"}, 403, "FORBIDDEN"},
			{"stranger read PAT, describe", s.WithPAT(sRead.Value), http.MethodGet, "/datapacks/" + id, nil, 404, "NOT_FOUND"},
			{"stranger read PAT, update (ownership group runs first)", s.WithPAT(sRead.Value), http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "x"}, 404, "NOT_FOUND"},
			{"stranger write PAT, update (then ownership)", s.WithPAT(sWrite.Value), http.MethodPatch, "/datapacks/" + id, map[string]string{"description": "x"}, 404, "NOT_FOUND"},
			{"owner PAT on a JWT-only route (provision-status)", a.WithPAT(all.Value), http.MethodGet, "/datapacks/" + id + "/provision-status", nil, 401, "UNAUTHORIZED"},
			{"owner PAT on a JWT-only route (list tokens)", a.WithPAT(all.Value), http.MethodGet, "/me/meko-tokens", nil, 401, "UNAUTHORIZED"},
			{"owner PAT minting a PAT", a.WithPAT(all.Value), http.MethodPost, "/me/meko-tokens", map[string]string{"name": "from-a-pat"}, 401, "UNAUTHORIZED"},
		}
		var errs []error
		for _, c := range cases {
			got, err := c.client.Raw(t, c.method, c.path, c.body)
			if err != nil {
				return err
			}
			if got.Status != c.status || (c.code != "" && got.Code != c.code) {
				errs = append(errs, fmt.Errorf("%s: HTTP %d %q, want %d %q: %s", c.who, got.Status, got.Code, c.status, c.code, truncateBody(got.Body)))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}

		if err := a.API.RevokeToken(t, read.Record.ID); err != nil {
			return err
		}
		return refuses(t, "a revoked PAT", a.WithPAT(read.Value), http.MethodGet, "/datapacks/"+id, statusUnauth, "UNAUTHORIZED")
	}); err != nil {
		return err
	}

	return t.Step("delete_is_the_owners_alone", func(*core.StepHandle) error {
		if err := a.API.DeleteDatapack(t, id); err != nil {
			return err
		}
		deleted = true
		for _, who := range by {
			if err := refuses(t, who.name+" after the delete", who.a.API, http.MethodGet, "/datapacks/"+id, statusNotFound, "NOT_FOUND"); err != nil {
				return err
			}
		}
		return nil
	})
}

type accountRef struct {
	a    *stacktest.Account
	name string
}
