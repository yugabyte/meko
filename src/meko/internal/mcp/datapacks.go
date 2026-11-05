package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/wire"
)

// Limits on datapack_search, as Cloud's.
const (
	maxQueryLen       = 200
	defaultSearchPage = 20
	maxSearchPage     = 100
)

func datapackTools() []*tool {
	pTrace := param{name: "conversation_id", typ: "string", def: "",
		desc: "The conversation this call belongs to: the id conversation_create returned."}
	pTarget := param{name: "datapack_id", typ: "string", required: true, desc: "The datapack's UUID."}
	return []*tool{
		{
			name: "datapack_list", title: "List Datapacks", kind: readOnly, failCode: "list_failed",
			desc: "List every datapack: Meko's separate spaces of memories, conversations and files.",
			params: []param{pTrace, pDatapack,
				{name: "query", typ: "string", def: "", desc: "Only datapacks whose name contains this."},
				{name: "limit", typ: "integer", def: 0, desc: "The most datapacks to return; 0 for all."}},
			run: datapackList,
		},
		{
			name: "datapack_search", title: "Search Datapacks", kind: readOnly, failCode: "search_failed",
			desc: "Find datapacks by name, a page at a time.",
			params: []param{pTrace,
				{name: "query", typ: "string", def: "", desc: "Text the name contains (200 characters at most)."},
				{name: "name", typ: "string", def: "", desc: "The exact name."},
				{name: "limit", typ: "integer", def: defaultSearchPage, desc: "The most to return (1 to 100)."},
				{name: "cursor", typ: "string", def: "", desc: "next_cursor from the page before."},
				{name: "grant", typ: "string", def: "", desc: "owner, shared or blank for both."},
				{name: "sort", typ: "string", def: "created", desc: "created (newest first) or name."},
				{name: "include_total", typ: "boolean", def: false, desc: "Also count every match."},
				pDatapack},
			run: datapackSearch,
		},
		{
			name: "datapack_create", title: "Create Datapack", kind: write, failCode: "create_failed",
			desc: "Create a datapack: a separate space of memories, conversations and files.",
			params: []param{
				{name: "name", typ: "string", required: true, desc: "Its name: 3 to 60 characters, unique."},
				pTrace, pDatapack,
				{name: "folder_id", typ: "string", def: "",
					desc: "A project folder to file it in (project_describe finds the id), checked before the " +
						"datapack is made; blank leaves it Unfiled."}},
			run: datapackCreate,
		},
		{
			name: "datapack_describe", title: "Describe Datapack", kind: readOnly, failCode: "describe_failed",
			desc: "Describe a datapack and count what it holds: memories, learnings (memories waiting to be kept " +
				"or promoted), collective memories and conversations, and say where it's filed: project, folder " +
				"and path (project/folder), each null when it's Unfiled.",
			params: []param{
				{name: "datapack_id", typ: "string", def: "", desc: "The datapack's UUID; blank for the default."},
				{name: "include_status", typ: "boolean", def: false, desc: "Also report its status."},
				pTrace},
			run: datapackDescribe,
		},
		{
			name: "datapack_metrics", title: "Datapack Metrics", kind: readOnly, failCode: "metrics_failed",
			desc: "A datapack's context metrics, as the web UI's Overview shows them: context size (the tokens " +
				"its knowledge base, memories and conversations hold), context efficiency (the tokens " +
				"context_search sent, against sending the whole knowledge base) and context relevance (the " +
				"share of search results scoring 0.5 or more). Without datapack_id it measures the default " +
				"datapack: ask the user which datapack they mean (datapack_list shows them) and tell them " +
				"when you use the default.",
			params: []param{pDatapack,
				{name: "days", typ: "integer", def: 0,
					desc: "Measure the searches of the last this many days; 0 for all the call log keeps (90)."},
				pTrace},
			run: datapackMetrics,
		},
		{
			name: "datapack_update", title: "Update Datapack", kind: write, failCode: "update_failed",
			desc: "Rename a datapack, change its description, or opt it out of conversation search (or back " +
				"in). A blank value leaves that field as it is.",
			params: []param{pTarget,
				{name: "name", typ: "string", nullable: true, desc: "The new name."},
				{name: "description", typ: "string", nullable: true, desc: "The new description."},
				{name: "conversation_search_opt_out", typ: "boolean", nullable: true,
					desc: "True stops new turns from being searchable; false makes them searchable again."},
				pTrace},
			run: datapackUpdate,
		},
		{
			name: "datapack_delete", title: "Delete Datapack", kind: destructive, failCode: "delete_failed",
			desc:   "Delete a datapack and everything in it, for good.",
			params: []param{pTarget, pTrace},
			run:    datapackDelete,
		},
	}
}

func datapackRows(ds []store.Datapack, userID string) []wire.Datapack {
	out := make([]wire.Datapack, len(ds))
	for i, d := range ds {
		out[i] = wire.DatapackRow(d, userID)
	}
	return out
}

func datapackList(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	query, err := a.str("query")
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 0)
	if err != nil {
		return nil, err
	}
	ds, err := c.svc.Datapacks(ctx)
	if err != nil {
		return nil, err
	}
	ds = matching(ds, query, "")
	sortDatapacks(ds, "created")
	if limit > 0 && len(ds) > limit {
		ds = ds[:limit]
	}
	return datapackRows(ds, c.svc.UserID()), nil
}

// matching keeps the datapacks whose name contains query (any case) and, if name is set, is name.
func matching(ds []store.Datapack, query, name string) []store.Datapack {
	out := []store.Datapack{}
	q := strings.ToLower(strings.TrimSpace(query))
	for _, d := range ds {
		if (q == "" || strings.Contains(strings.ToLower(d.Name), q)) && (name == "" || d.Name == name) {
			out = append(out, d)
		}
	}
	return out
}

// sortDatapacks orders datapacks newest first, or by name.
func sortDatapacks(ds []store.Datapack, by string) {
	sort.SliceStable(ds, func(i, j int) bool {
		if by == "name" {
			return strings.ToLower(ds[i].Name) < strings.ToLower(ds[j].Name)
		}
		return ds[i].CreatedAt.After(ds[j].CreatedAt)
	})
}

func datapackSearch(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	var query, name, cursor, grant, by string
	var err error
	for n, dst := range map[string]*string{"query": &query, "name": &name, "cursor": &cursor, "grant": &grant,
		"sort": &by} {
		if *dst, err = a.str(n); err != nil {
			return nil, err
		}
	}
	limit, err := a.integer("limit", defaultSearchPage)
	if err != nil {
		return nil, err
	}
	withTotal, err := a.boolean("include_total")
	if err != nil {
		return nil, err
	}
	switch {
	case utf8.RuneCountInString(query) > maxQueryLen:
		return nil, fail("invalid_query", "query must be 200 characters or fewer.")
	case grant != "" && grant != "owner" && grant != "shared":
		return nil, fail("invalid_grant", "grant must be owner, shared or blank.")
	case by != "" && by != "created" && by != "name":
		return nil, fail("invalid_sort", "sort must be created or name.")
	}
	offset := 0
	if cursor != "" {
		if offset, err = strconv.Atoi(cursor); err != nil || offset < 0 {
			return nil, fail("cursor_not_supported", "cursor must be a next_cursor this tool returned.")
		}
	}
	ds, err := c.svc.Datapacks(ctx)
	if err != nil {
		return nil, err
	}
	ds = matching(ds, query, name)
	if grant == "shared" { // the local user owns every datapack
		ds = nil
	}
	sortDatapacks(ds, by)
	limit = min(max(limit, 1), maxSearchPage)
	total := len(ds)
	start := min(offset, total) // before adding limit, so a huge cursor can't overflow
	end := min(start+limit, total)
	out := map[string]any{"datapacks": datapackRows(ds[start:end], c.svc.UserID()), "has_more": end < total,
		"next_cursor": "", "total": nil}
	if end < total {
		out["next_cursor"] = strconv.Itoa(end)
	}
	if withTotal {
		out["total"] = total
	}
	return out, nil
}

func datapackCreate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	name, err := a.str("name")
	if err != nil {
		return nil, err
	}
	folder, err := a.str("folder_id")
	if err != nil {
		return nil, err
	}
	d, err := c.svc.CreateDatapackIn(ctx, name, "", folder)
	if err != nil {
		return nil, projectToolError(err)
	}
	c.rec.DatapackID = d.ID
	return map[string]string{"datapack_id": d.ID, "agent_id": memory.DefaultAgentID, "name": d.Name}, nil
}

// datapackError is a datapack write's error with the codes Cloud's API gives them. Cloud's
// datapack_update and datapack_delete pass on its API's error, so a missing datapack is NOT_FOUND
// there rather than the datapack_access_denied other tools reply.
func datapackError(err error) error {
	switch {
	case errors.Is(err, memory.ErrNoDatapack):
		return fail("NOT_FOUND", "No such datapack.")
	case errors.Is(err, memory.ErrDuplicateName):
		return fail("CONFLICT", "A datapack with that name already exists.")
	case errors.Is(err, memory.ErrBadRequest):
		return fail("VALIDATION_ERROR", strings.TrimPrefix(err.Error(), memory.ErrBadRequest.Error()+": "))
	}
	return err
}

func datapackDescribe(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	withStatus, err := a.boolean("include_status")
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	d, err := c.svc.Datapack(ctx, dp)
	if err != nil {
		return nil, err
	}
	counts, err := c.svc.CountDatapack(ctx, dp)
	if err != nil {
		return nil, err
	}
	desc := wire.Describe(d, c.svc.UserID(), counts)
	placed, err := c.svc.Placements(ctx, []string{dp})
	if err != nil {
		return nil, err
	}
	filed := wire.FiledAs(placed[dp])
	if !withStatus {
		return struct {
			wire.DatapackDescription
			wire.FiledPath
		}{desc, filed}, nil
	}
	return struct {
		wire.DatapackDescription
		wire.FiledPath
		Status map[string]any `json:"status"`
	}{desc, filed, map[string]any{"agent_count": 0, "knowledge_base_count": desc.KnowledgeBaseFileCount,
		"status": "ready"}}, nil
}

// datapackMetrics is the Overview's three context tiles, from the same numbers /metrics gives the web
// UI. A percentage is null until there's something to measure.
func datapackMetrics(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	days, err := a.integer("days", 0)
	if err != nil {
		return nil, err
	}
	if days < 0 {
		return nil, fail("invalid_days", "days is 0 (all the call log keeps) or more.")
	}
	named, err := a.str("datapack_id")
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	st, err := c.svc.CallStats(ctx, dp, days)
	if err != nil {
		return nil, err
	}
	z, err := c.svc.SizeDatapack(ctx, dp)
	if err != nil {
		return nil, err
	}
	kb, err := c.svc.KnowledgeStats(ctx, dp)
	if err != nil {
		return nil, err
	}
	kept := int(memory.CallRetention.Hours() / 24)
	if days == 0 || days > kept {
		days = kept
	}
	cx := st.Context
	percent := func(part, whole int) *int {
		if whole == 0 {
			return nil
		}
		p := int(math.Round(100 * float64(part) / float64(whole)))
		return &p
	}
	d, err := c.svc.Datapack(ctx, dp)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"datapack_id": dp, "datapack_name": d.Name,
		"used_default": strings.TrimSpace(named) == "", "days": days,
		"searches":     cx.Requests,
		"context_size": memory.SizeOf(kb, z),
		"context_efficiency": map[string]any{"tokens_sent": cx.Sent, "tokens_saved": cx.Saved,
			"saved_pct": percent(cx.Saved, cx.IfRaw)},
		"context_relevance": map[string]any{"relevant_pct": percent(cx.ScoreHigh+cx.Mid, cx.ScoreHigh+cx.Mid+cx.Low),
			"high": cx.ScoreHigh, "mid": cx.Mid, "low": cx.Low},
	}, nil
}

func datapackUpdate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := a.str("datapack_id")
	if err != nil {
		return nil, err
	}
	if id = memory.CanonicalID(id); id == "" {
		return nil, fail("datapack_id_required", "Pass datapack_id (UUID). Use datapack_search(query=...) to find it.")
	}
	c.rec.DatapackID = id
	name, err := blankIsNil(a, "name")
	if err != nil {
		return nil, err
	}
	desc, err := blankIsNil(a, "description")
	if err != nil {
		return nil, err
	}
	optOut, err := a.optBool("conversation_search_opt_out")
	if err != nil {
		return nil, err
	}
	if name == nil && desc == nil && optOut == nil {
		return map[string]string{"result": "nothing_to_update", "detail": "Pass at least one of `name`, " +
			"`description`, or `conversation_search_opt_out` to update."}, nil
	}
	if _, err = c.svc.UpdateDatapack(ctx, id, name, desc, optOut); err != nil {
		return nil, datapackError(err)
	}
	return map[string]string{"message": "datapack updated"}, nil
}

// blankIsNil is a string argument, nil when absent, null or blank: Cloud's datapack_update treats
// a blank value as not given.
func blankIsNil(a args, name string) (*string, error) {
	s, err := a.optStr(name)
	if err != nil || s == nil || strings.TrimSpace(*s) == "" {
		return nil, err
	}
	return s, nil
}

func datapackDelete(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := a.str("datapack_id")
	if err != nil {
		return nil, err
	}
	if id = memory.CanonicalID(id); id == "" {
		return nil, fail("datapack_id_required", "Pass datapack_id (UUID). Use datapack_search(query=...) to find it.")
	}
	c.rec.DatapackID = id
	if err = c.svc.DeleteDatapack(ctx, id); err != nil {
		return nil, datapackError(err)
	}
	return map[string]string{"message": "datapack deleted"}, nil
}

// While Meko Cloud is linked, datapack_list and datapack_search add this Meko's datapacks to Meko
// Cloud's reply (cloud.go's routed), in the same row shape and with no word of where each lives: a
// call that names one runs here. A datapack both sides have that this Meko lists is listed once, as
// this Meko's; when this Meko's copy doesn't match the query, Meko Cloud's row stays. This Meko's
// rows carry no user_id: the user's account id is Meko Cloud's, and AgentK reads it from an owner
// row's user_id.

// cloudRowID is the id of one of Meko Cloud's datapack rows.
func cloudRowID(row json.RawMessage) string {
	var r struct {
		ID string `json:"datapack_id"`
	}
	_ = json.Unmarshal(row, &r)
	return r.ID
}

// localMatches is this Meko's datapacks that match query and name, sorted by, with no user_id, and
// their ids.
func (o Options) localMatches(ctx context.Context, query, name, by string) ([]wire.Datapack, map[string]bool, error) {
	ds, err := o.Service.ServedDatapacks(ctx)
	if err != nil {
		return nil, nil, err
	}
	ds = matching(ds, query, name)
	sortDatapacks(ds, by)
	ids := map[string]bool{}
	for _, d := range ds {
		ids[d.ID] = true
	}
	return datapackRows(ds, ""), ids, nil
}

// withoutIDs is rows without those whose id is in ids, and how many it left out.
func withoutIDs(rows []json.RawMessage, ids map[string]bool) ([]any, int) {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		if !ids[cloudRowID(r)] {
			out = append(out, r)
		}
	}
	return out, len(rows) - len(out)
}

// mergeList is datapack_list's reply while linked: Meko Cloud's rows, then this Meko's matching
// query, newest first, cut to limit. A reply that isn't a list passes as it came.
func (o Options) mergeList(ctx context.Context, a args, reply string) (any, error) {
	var rows []json.RawMessage
	if json.Unmarshal([]byte(reply), &rows) != nil {
		return nil, nil
	}
	query, _ := a.str("query")
	limit, _ := a.integer("limit", 0)
	local, ids, err := o.localMatches(ctx, query, "", "created")
	if err != nil {
		return nil, err
	}
	out, _ := withoutIDs(rows, ids)
	for _, d := range local {
		out = append(out, d)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// mergeSearch is datapack_search's page while linked. The first page (no cursor) has this Meko's
// matches first (every one, so the page can be longer than limit), then Meko Cloud's; a page asked
// for with a cursor is Meko Cloud's, whose cursor it is. Every page leaves out Meko Cloud's rows of
// the ids this Meko lists. A total Meko Cloud gave (include_total) counts this Meko's matches too,
// less the rows this page left out: one of an id on both sides on another page is counted twice. A
// reply that isn't a page passes as it came.
func (o Options) mergeSearch(ctx context.Context, a args, reply string) (any, error) {
	var page map[string]json.RawMessage
	var rows []json.RawMessage
	if json.Unmarshal([]byte(reply), &page) != nil || json.Unmarshal(page["datapacks"], &rows) != nil {
		return nil, nil
	}
	cursor, _ := a.str("cursor")
	query, _ := a.str("query")
	name, _ := a.str("name")
	grant, _ := a.str("grant")
	by, _ := a.str("sort")
	local, ids, err := o.localMatches(ctx, query, name, by)
	if err != nil {
		return nil, err
	}
	if grant == "shared" { // the local user owns every datapack here
		local, ids = nil, nil
	}
	cloudRows, dropped := withoutIDs(rows, ids)
	out := make([]any, 0, len(local)+len(cloudRows))
	if cursor == "" {
		for _, d := range local {
			out = append(out, d)
		}
	}
	merged := map[string]any{}
	for k, v := range page {
		merged[k] = v
	}
	merged["datapacks"] = append(out, cloudRows...)
	var total *int
	if json.Unmarshal(page["total"], &total) == nil && total != nil {
		merged["total"] = *total + len(local) - dropped
	}
	return merged, nil
}
