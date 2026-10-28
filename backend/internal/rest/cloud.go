package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
)

// The linked Meko Cloud on this Meko's own routes (docs/cloud.md): GET /datapacks lists its
// datapacks after this Meko's (location=all, the default; local or cloud lists one side), POST
// /datapacks with location "cloud" makes one there, and the routes that take a datapack in the path
// (delete, upload-url, document-status, search, memories) send a datapack this Meko doesn't have to
// it. The server calls Meko Cloud with the link's key, which never leaves it.

// owner says whether a request may act on the linked Meko Cloud: the admin token (mekosh) or the
// owner on the web listener. An agent's client token mayn't: the key acts on the whole Meko Cloud
// account. Listing its datapacks takes any credential.
func (a *api) owner(c echo.Context) bool {
	switch a.Credential(c) {
	case "admin", "session", "local":
		return true
	}
	return false
}

// inCloud says whether the datapack in the path is the linked Meko Cloud's: a UUID this Meko doesn't
// have, while Meko Cloud is linked. Anything else is this Meko's route's to answer (a 400 or 404 too).
func (a *api) inCloud(c echo.Context) (bool, error) {
	if a.Cloud == nil {
		return false, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil {
		return false, nil
	}
	if _, err = a.Service.Datapack(ctx(c), id.String()); !errors.Is(err, memory.ErrNoDatapack) {
		return false, nil
	}
	return a.Cloud.IsLinked()
}

// toCloud calls the linked Meko Cloud's REST API for the owner and replies with its data, at status.
func (a *api) toCloud(c echo.Context, status int, method, path string, body any) error {
	if !a.owner(c) {
		return forbiddenCloud(c)
	}
	data, err := a.Cloud.API(ctx(c), method, path, body)
	if err != nil {
		return cloudError(c, err)
	}
	return c.JSON(status, envelope.Response{Data: data})
}

func forbiddenCloud(c echo.Context) error {
	return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
		"Meko Cloud's datapacks take the owner or the admin token (mekosh), not a client token. "+
			"To make a datapack on this Meko, send location \"local\".")
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

// cloudPath is a Meko Cloud datapack's REST path, plus rest.
func cloudPath(c echo.Context, rest string) string {
	return "/datapacks/" + url.PathEscape(strings.TrimSpace(c.Param("id"))) + rest
}

// cloudRow is a Meko Cloud datapack as GET /datapacks?location= lists it.
type cloudRow struct {
	cloudlink.Datapack
	Location string `json:"location"`
}

// listLocation answers GET /datapacks?location=cloud (the linked Meko Cloud's datapacks, newest
// first; a 409 while unlinked) and location=all, the default (this Meko's, then Meko Cloud's while
// linked, each with its location). Meko Cloud's failures are 502s with their code.
func (a *api) listLocation(c echo.Context, location string, local []datapackRow) error {
	if a.Cloud == nil {
		if location == "all" {
			return envelope.OK(c, localRows(local))
		}
		return envelope.Fail(c, http.StatusConflict, cloudlink.CodeNotLinked, "This Meko has no Meko Cloud link.")
	}
	list, err := a.Cloud.Datapacks(ctx(c))
	if err != nil {
		return cloudError(c, err)
	}
	if !list.Linked && location == "cloud" {
		return envelope.Fail(c, http.StatusConflict, cloudlink.CodeNotLinked,
			"This Meko isn't linked to Meko Cloud. Link it in Settings › Meko Cloud, or with meko cloud_link.")
	}
	out := []any{}
	if location == "all" {
		out = localRows(local)
	}
	for _, d := range list.Datapacks {
		out = append(out, cloudRow{Datapack: d, Location: "cloud"})
	}
	return envelope.OK(c, out)
}

func localRows(rows []datapackRow) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct {
			datapackRow
			Location string `json:"location"`
		}{r, "local"})
	}
	return out
}

// createInCloud makes a datapack in the linked Meko Cloud, replying as this Meko's create does: only
// the id and name are copied from Meko Cloud's reply.
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
	return created(c, map[string]any{"datapack_id": d.ID, "agent_id": d.AgentID, "name": d.Name,
		"location": "cloud"})
}

// restConversation is the conversation id the search and memories routes pass when the caller gives
// none (Meko Cloud's tools require one): the same for every call, derived from "meko-rest".
var restConversation = func() string {
	sum := sha256.Sum256([]byte("meko-rest"))
	return hex.EncodeToString(sum[:16])
}()

// toolReply replies with what a tool answered: its reply as data, or its error a caller can act on
// ({"error", "detail"}) as a 400 with that code.
func toolReply(c echo.Context, text string) error {
	var e struct {
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(text), &e) == nil && e.Error != nil {
		code, _ := e.Error.(string)
		if code == "" {
			code = "TOOL_ERROR"
		}
		return envelope.Fail(c, http.StatusBadRequest, code, e.Detail)
	}
	return envelope.OK(c, json.RawMessage(text))
}

// callTool runs a tool on the datapack in the path: this Meko's, in-process as /mcp does, or the
// linked Meko Cloud's for the owner. args hold only arguments both take (Meko Cloud refuses others).
func (a *api) callTool(c echo.Context, name string, args map[string]any) error {
	if args["conversation_id"] == "" {
		args["conversation_id"] = restConversation
	}
	if args["agent_id"] == "" {
		delete(args, "agent_id")
	}
	cloud, err := a.inCloud(c)
	if err != nil {
		return err
	}
	if cloud {
		if !a.owner(c) {
			return forbiddenCloud(c)
		}
		args["datapack_id"] = strings.TrimSpace(c.Param("id"))
		text, terr := a.Cloud.Tool(ctx(c), name, args)
		if terr != nil {
			return cloudError(c, terr)
		}
		return toolReply(c, text)
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
	return toolReply(c, text)
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
