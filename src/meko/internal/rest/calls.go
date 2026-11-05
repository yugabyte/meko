package rest

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/mcp"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/wire"
)

// The Calls page's page sizes.
const (
	defaultCallPage = 50
	maxCallPage     = 200
)

// listCalls is GET /calls: the call log, newest first, this Meko's calls and the ones it sent on to
// a linked Meko Cloud alike, filtered by agent, conversation, datapack, tool, client (agentk) and failed=true, a
// page at a time (before: the last id of the page before).
func (a *api) listCalls(c echo.Context) error {
	limit, ok, err := queryInt(c, "limit", defaultCallPage)
	if !ok {
		return err
	}
	before, ok, err := queryInt(c, "before", 0)
	if !ok {
		return err
	}
	if limit < 1 || limit > maxCallPage || before < 0 {
		return badRequest(c, "limit is 1 to 200, and before an id")
	}
	f := store.CallFilter{AgentID: c.QueryParam("agent"), ConversationID: c.QueryParam("conversation"),
		DatapackID: c.QueryParam("datapack"), Tool: c.QueryParam("tool"), Client: c.QueryParam("client"),
		Failed: c.QueryParam("failed") == "true", Before: int64(before)}
	calls, more, err := a.Service.ListCalls(ctx(c), f, limit)
	if err != nil {
		return err
	}
	out := make([]map[string]any, len(calls))
	for i, r := range calls {
		out[i] = callJSON(r)
	}
	return envelope.OK(c, map[string]any{"calls": out, "more": more})
}

// call is GET /calls/:id: one call, with what it sent (args) and got back (reply) when they're kept.
func (a *api) call(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		return badRequest(c, "id is a call's number")
	}
	r, cc, err := a.Service.Call(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	out := callJSON(r)
	if cc != nil {
		out["args"], out["reply"], out["cut"] = cc.Args, cc.Reply, cc.Cut
	}
	return envelope.OK(c, out)
}

// callJSON is a call as the Calls page shows it: /usage's recent call plus the client that sent it,
// its ids and whether its contents are kept. Like every listing, it doesn't say which side a call ran
// on.
func callJSON(r store.Call) map[string]any {
	return map[string]any{"id": r.ID, "steps": r.Steps, "at": wire.Time(r.At), "tool": r.Tool,
		"agent_id": r.AgentID, "ok": r.OK, "error_code": r.ErrorCode, "ms": r.Ms, "memory_n": r.MemoryN,
		"kb_n": r.KBN, "conversation_n": r.ConversationN, "read_only": mcp.ReadOnly(r.Tool),
		"datapack_id": r.DatapackID, "conversation_id": r.ConversationID, "client": r.Client,
		"has_contents": r.HasContents}
}

// callContents is GET /settings/call-contents: whether calls' contents are kept, for how long, and
// how many are kept now and their size.
func (a *api) callContents(c echo.Context) error {
	st, err := a.Service.CallContentsState(ctx(c))
	if err != nil {
		return err
	}
	return envelope.OK(c, st)
}

// setCallContents is PUT /settings/call-contents {keep}, for the owner only (Settings, or the CLI's
// admin token): what it keeps is every agent's queries and Meko's replies.
func (a *api) setCallContents(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only Meko's owner can change this, in Settings.")
	}
	var body struct {
		Keep *bool `json:"keep"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if body.Keep == nil {
		return badRequest(c, "keep is true or false")
	}
	if err := a.Service.SetKeepCallContents(ctx(c), *body.Keep); err != nil {
		return err
	}
	return a.callContents(c)
}

// clearCallContents is DELETE /calls/contents, for the owner only: every call's kept contents go, and
// the calls stay.
func (a *api) clearCallContents(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only Meko's owner can clear them, in Settings.")
	}
	if err := a.Service.ClearCallContents(ctx(c)); err != nil {
		return err
	}
	return a.callContents(c)
}
