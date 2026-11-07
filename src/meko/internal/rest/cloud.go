package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
)

// The linked Meko Cloud on this Meko's own routes (docs/cloud.md): GET /datapacks lists its
// datapacks with this Meko's, POST /datapacks makes one there, the routes that take a datapack in
// the path and that Meko Cloud has too send a datapack this Meko doesn't have to it (byID), as /mcp
// does, and the routes only Meko Cloud has (sharing) go there (cloudOnly). No route takes or says a
// location: the datapack's id decides. The server calls Meko Cloud with the link's key, which never
// leaves it.

// owner says whether the request is the owner's: the admin token (meko-cli) or a browser on the web
// listener (local or remote access), not an agent's client token. The owner's settings and a create
// in the linked Meko Cloud take it; a client creates one through /mcp.
func (a *api) owner(c echo.Context) bool {
	switch a.Credential(c) {
	case "admin", "remote", "local":
		return true
	}
	return false
}

// inCloud says whether the datapack in the path is the linked Meko Cloud's: an id this Meko doesn't
// have is, while it's linked, for any credential the data routes take, as on /mcp. A datapack this
// Meko has is this Meko's, even when Meko Cloud has the same id, and unlinked a missing id is this
// Meko's 404. Anything this Meko's is its route's to answer (a 400 or 404 too). ok is false when the
// reply has been written.
func (a *api) inCloud(c echo.Context) (cloud, ok bool, err error) {
	if a.Cloud == nil || a.staleTab(c) {
		return false, true, nil
	}
	id, perr := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if perr != nil {
		return false, true, nil
	}
	// A failed lookup is this Meko's to answer too: its route then fails here, never in Meko Cloud.
	if here, herr := a.Service.ServesHere(ctx(c), id.String()); here || herr != nil {
		return false, true, nil
	}
	linked, err := a.Cloud.IsLinked()
	return linked, err == nil, err
}

// staleTab says whether the request is a web UI tab still running a bundle from before these routes
// took no location: a browser (local or remote access) sending ?location=local, which only those
// bundles send. Such a tab takes every row it lists as this Meko's and opens it here, and holds its
// search and memories to this Meko, so it's answered as before: this Meko's datapacks alone, and its
// :id routes never sent to Meko Cloud. Any other location, and any from a token, is ignored.
func (a *api) staleTab(c echo.Context) bool {
	return a.browser(c) && c.QueryParam("location") == "local"
}

// browser says whether the request is a browser's on the web listener (local or remote access).
func (a *api) browser(c echo.Context) bool {
	switch a.Credential(c) {
	case "remote", "local":
		return true
	}
	return false
}

// linked says whether Meko Cloud is linked.
func (a *api) linked() (bool, error) {
	if a.Cloud == nil {
		return false, nil
	}
	return a.Cloud.IsLinked()
}

// notLinkedOr replies 409 CLOUD_NOT_LINKED, or returns err when the link couldn't be read.
func notLinkedOr(c echo.Context, err error) error {
	if err != nil {
		return err
	}
	return envelope.Fail(c, http.StatusConflict, cloudlink.CodeNotLinked,
		"This Meko isn't linked to Meko Cloud. Link it in Settings › Meko Cloud, or with meko cloud_link.")
}

// byID serves a route that takes a datapack in the path and that Meko Cloud's REST API has too: h for
// a datapack this Meko has, and Meko Cloud's same route for one it doesn't while it's linked
// (inCloud), passed on as it came (pass). A browser's stays here: the web UI opens Meko Cloud's
// datapacks in its web app, never on this Meko's pages.
func (a *api) byID(h echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if a.browser(c) {
			return h(c)
		}
		cloud, ok, err := a.inCloud(c)
		if !ok {
			return err
		}
		if cloud {
			return a.pass(c)
		}
		return h(c)
	}
}

// codeDatapackLocal is a sharing route for a datapack this Meko has: only Meko Cloud shares.
const codeDatapackLocal = "DATAPACK_LOCAL"

// cloudOnly answers a route only Meko Cloud has (sharing and invites): passed on to it while it's
// linked, and a 409 CLOUD_NOT_LINKED otherwise. A datapack in the path must be a UUID, as on every
// route, and one that this Meko has is a 409 DATAPACK_LOCAL, since it can't be shared until it's in
// Meko Cloud.
func (a *api) cloudOnly(c echo.Context) error {
	linked, err := a.linked()
	if err != nil || !linked {
		return notLinkedOr(c, err)
	}
	if slices.Contains(c.ParamNames(), "id") {
		u, perr := uuid.Parse(strings.TrimSpace(c.Param("id")))
		if perr != nil {
			return badRequest(c, "datapack_id must be a valid UUID")
		}
		if here, herr := a.Service.ServesHere(ctx(c), u.String()); herr != nil {
			return herr
		} else if here {
			return envelope.Fail(c, http.StatusConflict, codeDatapackLocal,
				"This datapack is on this Meko, not in Meko Cloud. It can be shared once it's in Meko Cloud.")
		}
	}
	return a.pass(c)
}

func forbiddenCloud(c echo.Context) error {
	return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
		"A datapack is made in Meko Cloud here by the owner or the admin token (meko-cli), not a client "+
			"token: a client makes one through /mcp.")
}

// cloudError replies for a call Meko Cloud didn't make: its status (a 502 unless Meko Cloud answered
// with a 4xx of its own, or a 409 when there's no link) and code, never a 401, which the web UI would
// read as its own sign-in lost. Any other error is internal.
func cloudError(c echo.Context, err error) error {
	var failed *cloudlink.CallError
	if !errors.As(err, &failed) {
		return err
	}
	status := failed.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	return envelope.Fail(c, status, failed.Code, failed.Error())
}

// withCloud replies to GET /datapacks with this Meko's rows and, while Meko Cloud is linked, its own,
// leaving out one whose id this Meko has: it's served here. All are sorted by name. Meko Cloud's
// failures are 502s with their code, as there's no whole list without it.
func (a *api) withCloud(c echo.Context, local []datapackRow) error {
	type named struct {
		name string
		row  any
	}
	rows := make([]named, 0, len(local))
	ids := map[string]bool{}
	for _, r := range local {
		rows = append(rows, named{r.DatapackName, r})
		ids[r.DatapackID] = true
	}
	if a.Cloud != nil {
		list, err := a.Cloud.Datapacks(ctx(c))
		if err != nil {
			return cloudError(c, err)
		}
		for _, d := range list.Datapacks {
			if !ids[d.DatapackID] {
				rows = append(rows, named{d.DatapackName, d})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return strings.ToLower(rows[i].name) < strings.ToLower(rows[j].name) })
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r.row
	}
	return envelope.OK(c, out)
}

// createInCloud makes a datapack in the linked Meko Cloud, replying as this Meko's create does: only
// the id, name and agent id are copied from Meko Cloud's reply, with web_url, the datapack in the
// link's web app (null when that isn't known).
func (a *api) createInCloud(c echo.Context, name, description string) error {
	if !a.owner(c) {
		return forbiddenCloud(c)
	}
	body := map[string]string{"name": name}
	if description != "" {
		body["description"] = description
	}
	data, err := a.Cloud.API(ctx(c), http.MethodPost, "/datapacks", body)
	if err != nil {
		return cloudError(c, err)
	}
	var d struct {
		ID      string `json:"datapack_id"`
		Name    string `json:"name"`
		AgentID string `json:"agent_id"`
	}
	if err = json.Unmarshal(data, &d); err != nil || d.ID == "" {
		return envelope.Fail(c, http.StatusBadGateway, cloudlink.CodeCallFailed,
			"Meko Cloud's reply to the create isn't a datapack.")
	}
	var web *string
	if st, serr := a.Cloud.Status(); serr == nil && st.EffectiveWebURL != nil {
		u := *st.EffectiveWebURL + "/datapacks/" + d.ID
		web = &u
	}
	return created(c, map[string]any{"datapack_id": d.ID, "agent_id": d.AgentID, "name": d.Name, "web_url": web})
}

// restConversation is the conversation id the search and memories routes pass when the caller gives
// none (Meko Cloud's tools require one): the same for every call, derived from "meko-rest".
var restConversation = func() string {
	sum := sha256.Sum256([]byte("meko-rest"))
	return hex.EncodeToString(sum[:16])
}()

// toolReply replies with what a tool answered: its reply as data, or its error reply ({"error",
// "detail"}) by its code. A missing datapack (datapack_access_denied, not_found) is a 404 NOT_FOUND,
// as on every route. A code ending in _failed is the tool's own failure (its failCode, Meko Cloud's
// as Meko's), a 500 here, whose cause the server's log has, or a 502 CLOUD_CALL_FAILED from Meko
// Cloud; context_engine_unavailable is a 503, and storage_limit_exceeded the 402 every route answers
// past the storage limit. Any other is a 400 with the tool's code.
func toolReply(c echo.Context, name, text string, cloud bool) error {
	var e struct {
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(text), &e) != nil || e.Error == nil {
		return envelope.OK(c, json.RawMessage(text))
	}
	code, _ := e.Error.(string)
	switch {
	case code == "datapack_access_denied" || code == "not_found" || strings.HasSuffix(code, "_not_found"):
		return notFound(c, "datapack not found")
	case strings.HasSuffix(code, "_failed") && cloud:
		return envelope.Fail(c, http.StatusBadGateway, cloudlink.CodeCallFailed,
			"Meko Cloud's "+name+" failed: "+strings.TrimSuffix(e.Detail, ".")+".")
	case strings.HasSuffix(code, "_failed"):
		return envelope.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", e.Detail)
	case code == "context_engine_unavailable":
		return envelope.Fail(c, http.StatusServiceUnavailable, code, e.Detail)
	case code == "storage_limit_exceeded":
		return envelope.Fail(c, http.StatusPaymentRequired, "STORAGE_LIMIT_EXCEEDED", e.Detail)
	case code == "":
		code = "TOOL_ERROR"
	}
	return envelope.Fail(c, http.StatusBadRequest, code, e.Detail)
}

// callTool runs a tool on the datapack in the path: this Meko's, in-process as /mcp does, or the
// linked Meko Cloud's (inCloud). args hold only arguments both take (Meko Cloud refuses others).
func (a *api) callTool(c echo.Context, name string, args map[string]any) error {
	if args["conversation_id"] == "" {
		args["conversation_id"] = restConversation
	}
	if args["agent_id"] == "" {
		delete(args, "agent_id")
	}
	cloud, ok, err := a.inCloud(c)
	if !ok {
		return err
	}
	if cloud {
		args["datapack_id"] = strings.TrimSpace(c.Param("id"))
		text, terr := a.Cloud.Tool(ctx(c), name, args)
		if terr != nil {
			return cloudError(c, terr)
		}
		return toolReply(c, name, text, true)
	}
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if a.Tool == nil {
		return envelope.Fail(c, http.StatusNotFound, "NOT_FOUND", "This Meko serves no tools here.")
	}
	args["datapack_id"] = id
	text, err := a.Tool(ctx(c), a.Credential(c), name, args)
	if err != nil {
		return badRequest(c, err.Error())
	}
	return toolReply(c, name, text, false)
}

// search is POST /datapacks/:id/search (Meko's own): context_search on the datapack, this Meko's or
// the linked Meko Cloud's, whose REST API has no search route.
func (a *api) search(c echo.Context) error {
	var body struct {
		Query          string `json:"query"`
		Limit          int    `json:"limit"`
		ForceSource    string `json:"force_source"`
		AgentID        string `json:"agent_id"`
		ConversationID string `json:"conversation_id"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if strings.TrimSpace(body.Query) == "" {
		return badRequest(c, "query is required")
	}
	args := map[string]any{"query": body.Query, "agent_id": body.AgentID, "conversation_id": body.ConversationID}
	if body.Limit > 0 {
		args["limit"] = body.Limit
	}
	if body.ForceSource != "" {
		args["force_source"] = body.ForceSource
	}
	return a.callTool(c, "context_search", args)
}

// addMemory is POST /datapacks/:id/memories (Meko's own): memory_add on the datapack, this Meko's or
// the linked Meko Cloud's, whose REST API has no route that adds a memory.
func (a *api) addMemory(c echo.Context) error {
	var body struct {
		Text           string `json:"text"`
		AgentID        string `json:"agent_id"`
		ConversationID string `json:"conversation_id"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if strings.TrimSpace(body.Text) == "" {
		return badRequest(c, "text is required")
	}
	return a.callTool(c, "memory_add", map[string]any{"text": strings.TrimSpace(body.Text),
		"agent_id": body.AgentID, "conversation_id": body.ConversationID})
}

// apiPrefix is where the routes are served: pass sends the path after it to Meko Cloud.
const apiPrefix = "/api/v1"

// pass sends the request to the linked Meko Cloud's REST API at the same path after /api/v1, with
// its method, query and body, and the link's key, and Meko Cloud's status, content type and body
// come back, without the credentials a JSON body holds (cloudlink.Service.Pass). A call that doesn't
// reach Meko Cloud is a 502 with its code. A path parameter that could name another path there (.,
// .., or one holding a slash, a backslash or an escape: Echo routes on the raw path) is a 400, so
// only the routes listed reach Meko Cloud with the key.
func (a *api) pass(c echo.Context) error {
	for i, n := range c.ParamNames() {
		if v := c.ParamValues()[i]; v == "." || v == ".." || strings.ContainsAny(v, `/\%`) {
			return badRequest(c, n+" isn't a valid path segment")
		}
	}
	r := c.Request()
	path := strings.TrimPrefix(r.URL.EscapedPath(), apiPrefix)
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.ContentLength != 0 && r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}
	reply, err := a.Cloud.Pass(ctx(c), r.Method, path, body, r.Header.Get("Content-Type"))
	if err != nil {
		return cloudError(c, err)
	}
	ct := reply.ContentType
	if ct == "" {
		ct = echo.MIMEApplicationJSON
	}
	return c.Blob(reply.Status, ct, reply.Body)
}
