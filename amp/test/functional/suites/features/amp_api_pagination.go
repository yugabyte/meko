// Every list route clamps limit to 1..1000 and offset to >= 0 instead of
// refusing (handlers.go pageParams), and `total` counts the whole collection
// while `count` counts the page. Nothing in the suite exercised the clamp or
// walked a collection one row at a time; the ListAll helpers page at 100.
package features

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// paginationBranches is how many branches are forked so the branch list spans
// several single-row pages. Branches with no endpoint are cheap.
const paginationBranches = 3

func init() {
	core.Register(core.Test{
		Name:        "features/amp_api_pagination",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Pagination contract on every list route (pools, clusters, branches, endpoints, tasks): limit=0 and limit=1001 are clamped not refused, a negative offset reads as 0, an offset past the end is an empty page with the same total, and walking a collection one row per page yields every id exactly once with a total that never moves.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     35 * time.Minute,
		Run:         runAmpAPIPagination,
	})
}

func runAmpAPIPagination(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"pagination", a.pagination}}
	})
}

// listRoute is one paginated collection: where it lives, the field its rows
// carry an id in, and whether this test is its only writer.
//
// `exclusive` is the difference between the two halves of the contract. On a
// shared control plane the pools and the pool's clusters are written by every
// other test running beside this one, so their totals legitimately move and only
// the clamp can be asserted. The branches of this test's own cluster, the
// endpoints of its own branch and the tasks filtered to its own cluster have no
// other writer, so those get the walk as well.
type listRoute struct {
	name      string
	path      string
	items     string
	idKey     string
	query     url.Values
	exclusive bool
}

// page is what every list route answers, read generically.
type page struct {
	ids   []string
	total int
	count int
}

func (a *ampFeatureRun) pagination(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	// The endpoint is what gives the parent a fork point (a branch with no
	// compute has none the controller can resolve), and it also puts a row in the
	// endpoints list this test walks.
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create the writer the fork point comes from: %w", err)
	}
	conn, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the writer: %w", err)
	}
	lsn, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}
	for i := 0; i < paginationBranches; i++ {
		name := fmt.Sprintf("page-%s-%d", a.f.suffix(), i)
		if err := createBranchRetrying(ctx, cl, a.proj.poolID, a.proj.projID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: a.proj.branchID, ForkPointLSN: lsn,
		}); err != nil {
			return fmt.Errorf("fork %s: %w", name, err)
		}
	}
	base := "/storage-pools/" + a.proj.poolID + "/clusters/" + a.proj.projID
	routes := []listRoute{
		{name: "storage pools", path: "/storage-pools", items: "storage_pools", idKey: "id"},
		{name: "clusters", path: "/storage-pools/" + a.proj.poolID + "/clusters", items: "clusters", idKey: "id"},
		{name: "branches", path: base + "/branches", items: "branches", idKey: "branch_id", exclusive: true},
		{name: "endpoints", path: base + "/branches/" + a.proj.branchID + "/endpoints", items: "endpoints", idKey: "id", exclusive: true},
		{name: "tasks", path: "/tasks", items: "tasks", idKey: "id", query: vals("cluster_id", a.proj.projID), exclusive: true},
	}
	for _, r := range routes {
		if err := paginationContract(ctx, cl, r); err != nil {
			return fmt.Errorf("%s (%s): %w", r.name, r.path, err)
		}
		a.f.logf("%s: pagination contract holds", r.name)
	}
	return nil
}

func getPage(ctx context.Context, cl *controller.Client, r listRoute, q url.Values) (page, error) {
	full := url.Values{}
	for k, v := range r.query {
		full[k] = v
	}
	for k, v := range q {
		full[k] = v
	}
	q = full
	var raw map[string]json.RawMessage
	status, err := cl.GetRaw(ctx, r.path, q, &raw)
	if err != nil {
		return page{}, fmt.Errorf("GET ?%s: HTTP %d: %w", q.Encode(), status, err)
	}
	var p page
	if err := json.Unmarshal(raw["total"], &p.total); err != nil {
		return page{}, fmt.Errorf("?%s: total is %s: %w", q.Encode(), raw["total"], err)
	}
	if err := json.Unmarshal(raw["count"], &p.count); err != nil {
		return page{}, fmt.Errorf("?%s: count is %s: %w", q.Encode(), raw["count"], err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw[r.items], &rows); err != nil {
		return page{}, fmt.Errorf("?%s: %q is %s: %w", q.Encode(), r.items, raw[r.items], err)
	}
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row[r.idKey], &id); err != nil || id == "" {
			return page{}, fmt.Errorf("?%s: a row has no %q: %s", q.Encode(), r.idKey, row)
		}
		p.ids = append(p.ids, id)
	}
	if p.count != len(p.ids) {
		return page{}, fmt.Errorf("?%s: count=%d but the page carries %d rows", q.Encode(), p.count, len(p.ids))
	}
	return p, nil
}

func vals(kv ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q
}

// paginationContract holds the clamp, the past-the-end page and the one-row
// walk for one route.
func paginationContract(ctx context.Context, cl *controller.Client, r listRoute) error {
	first, err := getPage(ctx, cl, r, nil)
	if err != nil {
		return err
	}
	total := first.total
	if total < len(first.ids) {
		return fmt.Errorf("total=%d is below the %d rows the default page returned", total, len(first.ids))
	}

	// limit=0 clamps to one row, limit=1001 to the 1000 cap; neither is a 400.
	zero, err := getPage(ctx, cl, r, vals("limit", "0"))
	if err != nil {
		return fmt.Errorf("limit=0 must be clamped to 1, not refused: %w", err)
	}
	if want := min(1, total); zero.count != want {
		return fmt.Errorf("limit=0 returned %d rows, want %d (clamped to 1)", zero.count, want)
	}
	over, err := getPage(ctx, cl, r, vals("limit", "1001"))
	if err != nil {
		return fmt.Errorf("limit=1001 must be clamped to 1000, not refused: %w", err)
	}
	if want := min(1000, total); over.count != want {
		return fmt.Errorf("limit=1001 returned %d rows, want %d", over.count, want)
	}
	if r.exclusive && over.total != total {
		return fmt.Errorf("total moved between calls though nothing else writes this collection: %d then %d", total, over.total)
	}

	// A negative offset reads as 0; an offset past the end is empty, same total.
	neg, err := getPage(ctx, cl, r, vals("offset", "-1", "limit", "1"))
	if err != nil {
		return fmt.Errorf("offset=-1 must be clamped to 0, not refused: %w", err)
	}
	if total > 0 && (len(neg.ids) != 1 || neg.ids[0] != zero.ids[0]) {
		return fmt.Errorf("offset=-1 returned %v, want the first row %v", neg.ids, zero.ids)
	}
	past, err := getPage(ctx, cl, r, vals("offset", strconv.Itoa(pastOffset(total))))
	if err != nil {
		return fmt.Errorf("an offset past the end must be an empty 200: %w", err)
	}
	if past.count != 0 {
		return fmt.Errorf("offset=%d past a total of %d returned %d rows, want an empty page", pastOffset(total), total, past.count)
	}

	if !r.exclusive {
		return nil
	}

	// One row per page: every id once, total constant on every page. Only for a
	// collection this test owns; bounded at total+1 pages so a route that ignores
	// offset cannot loop forever.
	seen := map[string]int{}
	walked := 0
	for off := 0; off < total && off <= total+1; off++ {
		p, err := getPage(ctx, cl, r, vals("limit", "1", "offset", strconv.Itoa(off)))
		if err != nil {
			return fmt.Errorf("walk offset=%d: %w", off, err)
		}
		if p.total != total {
			return fmt.Errorf("walk offset=%d: total moved to %d from %d with no writes in between", off, p.total, total)
		}
		if p.count != 1 {
			return fmt.Errorf("walk offset=%d of %d: got %d rows, want 1", off, total, p.count)
		}
		seen[p.ids[0]]++
		walked++
	}
	if walked != total {
		return fmt.Errorf("walked %d pages for a total of %d", walked, total)
	}
	for id, n := range seen {
		if n != 1 {
			return fmt.Errorf("id %s appeared on %d single-row pages: the ordering is not stable across offsets, so a paging client sees duplicates and misses rows", id, n)
		}
	}
	if len(seen) != total {
		return fmt.Errorf("the walk saw %d distinct ids for a total of %d", len(seen), total)
	}
	return nil
}

// pastOffset is an offset comfortably past the end even if the collection grew
// between calls.
func pastOffset(total int) int { return total + 50 }
