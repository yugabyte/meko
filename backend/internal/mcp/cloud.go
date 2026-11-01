package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/knowledge"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
)

// Cloud is the linked Meko Cloud as /mcp uses it (cloudlink.Service): while it's linked, a call that
// names one of this Meko's datapacks runs here, and every other call goes there.
type Cloud interface {
	IsLinked() (bool, error)
	// CloudTools is what Meko Cloud offers, nil when it couldn't be read.
	CloudTools(ctx context.Context) *cloudlink.Tools
	// Forward calls one of Meko Cloud's tools with the arguments as they came.
	Forward(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error)
}

// cloudInstructions is what initialize says while Meko Cloud is linked, until its own instructions
// are read.
const cloudInstructions = `Meko is your memory: facts, conversations and files that last across sessions and are
shared by every agent that uses the same datapack. This server is Meko on this machine, linked to Meko Cloud.

Start with context_search. Save a fact with memory_add. To add a file on this machine, pass its path to
knowledgebase_upload_file. Pass the same conversation_id to every call of a session. datapack_list shows the
datapacks; a blank datapack_id uses the default one.`

// cloudServers are the servers /mcp answers with while Meko Cloud is linked, one per credential,
// made again when Meko Cloud's tool list changes.
type cloudServers struct {
	mu      sync.Mutex
	tools   *cloudlink.Tools // what they were made from; nil: this Meko's own list of Cloud's tools
	made    bool
	servers map[string]*sdk.Server
}

// cloudServerFor is the server for a credential, made for Meko Cloud's tools as they are now.
func (o Options) cloudServerFor(ctx context.Context, cs *cloudServers, cred string) *sdk.Server {
	t := o.Cloud.CloudTools(ctx)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.made || cs.tools != t {
		cs.tools, cs.made, cs.servers = t, true, map[string]*sdk.Server{}
	}
	if s, ok := cs.servers[cred]; ok {
		return s
	}
	s := o.newCloudServer(t, cred)
	cs.servers[cred] = s
	return s
}

// newCloudServer is a server with Meko Cloud's tools and this Meko's knowledgebase_upload_file, which
// reads a file on this machine. A call that names a datapack this Meko has runs here, with this
// Meko's tool of that name; any other is sent on to Meko Cloud as it came (routed). Without Meko
// Cloud's list, it has the tools this Meko shares with Meko Cloud.
func (o Options) newCloudServer(t *cloudlink.Tools, cred string) *sdk.Server {
	instr := cloudInstructions
	var tools []*sdk.Tool
	if t != nil {
		if t.Instructions != "" {
			instr = t.Instructions
		}
		tools = t.Tools
	} else {
		for _, lt := range allTools() {
			if !localOnly[lt.name] {
				tools = append(tools, lt.sdkTool())
			}
		}
	}
	local := map[string]*tool{}
	for _, lt := range allTools() {
		local[lt.name] = lt
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "Meko", Version: o.Version}, &sdk.ServerOptions{Instructions: instr})
	for _, ct := range tools {
		// A path is this machine's: Meko Cloud's tool of that name, if it had one, would read its own.
		if ct == nil || ct.InputSchema == nil || ct.Name == uploadFileTool.name {
			continue
		}
		addTool(s, ct, o.routed(ct.Name, local[ct.Name], cred), o)
	}
	addTool(s, uploadFileTool.sdkTool(), o.byDatapack(uploadFileTool, cred, o.cloudUploadFile(cred)), o)
	return s
}

// routed is the handler for Meko Cloud's tool name while linked. datapack_list and datapack_search
// add this Meko's datapacks to Meko Cloud's reply (mergeList, mergeSearch). A tool this Meko has that
// works in one datapack runs here when the call names one of this Meko's (byDatapack). Anything else
// (datapack_create, a blank datapack_id, a tool only Meko Cloud has) goes to Meko Cloud.
func (o Options) routed(name string, lt *tool, cred string) sdk.ToolHandler {
	switch {
	case lt == nil || name == "datapack_create":
		return o.forwarder(name, cred)
	case name == "datapack_list":
		return o.forwardAnd(name, lt.failCode, cred, o.mergeList)
	case name == "datapack_search":
		return o.forwardAnd(name, lt.failCode, cred, o.mergeSearch)
	}
	for _, p := range lt.params {
		if p.name == "datapack_id" {
			return o.byDatapack(lt, cred, o.forwarder(name, cred))
		}
	}
	return o.forwarder(name, cred)
}

// byDatapack runs t here when the call's datapack_id is one this Meko has (servedHere), and hands it
// to elsewhere otherwise. When that can't be told, the call fails here: it's never sent to Meko Cloud
// on a guess.
func (o Options) byDatapack(t *tool, cred string, elsewhere sdk.ToolHandler) sdk.ToolHandler {
	here := o.handler(t, cred)
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		start := time.Now()
		ok, err := o.servedHere(ctx, req.Params.Arguments)
		switch {
		case err != nil:
			o.Log.Error("can't tell whether this Meko has a datapack", "tool", t.name, "error", err)
			rec := forwardedCall(start, t.name, cred, req.Params.Arguments)
			rec.Location, rec.ErrorCode = "", t.failCode
			rec.Ms = time.Since(start).Milliseconds()
			o.Service.RecordCall(ctx, rec)
			return textResult(fail(t.failCode, genericDetail)), nil
		case ok:
			return here(ctx, req)
		}
		return elsewhere(ctx, req)
	}
}

// servedHere says whether a call's datapack_id names a datapack this Meko serves while Meko Cloud is
// linked: one its store has. An id on both sides is this Meko's until it's synced. Arguments that
// don't name one (blank, or not an object) are Meko Cloud's to answer.
func (o Options) servedHere(ctx context.Context, raw json.RawMessage) (bool, error) {
	a, err := parseArgs(raw)
	if err != nil {
		return false, nil
	}
	id, err := a.str("datapack_id")
	if id = strings.TrimSpace(id); err != nil || id == "" {
		return false, nil
	}
	_, err = o.Service.Datapack(ctx, id)
	if errors.Is(err, memory.ErrNoDatapack) {
		return false, nil
	}
	return err == nil, err
}

// addTool adds a tool, skipping (and logging) one whose schema the SDK refuses: Meko Cloud's list
// isn't this Meko's to vouch for.
func addTool(s *sdk.Server, t *sdk.Tool, h sdk.ToolHandler, o Options) {
	defer func() {
		if r := recover(); r != nil {
			o.Log.Warn("left out one of Meko Cloud's tools", "tool", t.Name, "why", r)
		}
	}()
	s.AddTool(t, h)
}

// localOnly are this Meko's tools Meko Cloud doesn't have: datapack_metrics reads this Meko's call
// log, and knowledgebase_upload_file is this Meko's own while linked too (cloudUploadFile).
var localOnly = map[string]bool{"datapack_metrics": true, "knowledgebase_upload_file": true}

// uploadFileTool is knowledgebase_upload_file's definition.
var uploadFileTool = func() *tool {
	for _, t := range knowledgeTools() {
		if t.name == "knowledgebase_upload_file" {
			return t
		}
	}
	panic("no knowledgebase_upload_file")
}()

// forwarder sends a call on to Meko Cloud as it came and answers with its result, recording it in
// the call log as Meko Cloud's.
func (o Options) forwarder(name, cred string) sdk.ToolHandler {
	return o.forwardAnd(name, "", cred, nil)
}

// merger adds to a reply Meko Cloud answered without an error, given the call's arguments.
type merger func(ctx context.Context, a args, reply string) (any, error)

// forwardAnd is forwarder, with Meko Cloud's reply, when it isn't an error, passed through merge.
// When merge fails, the call fails with failCode.
func (o Options) forwardAnd(name, failCode, cred string, merge merger) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		start := time.Now()
		rec := forwardedCall(start, name, cred, req.Params.Arguments)
		res, err := o.Cloud.Forward(ctx, name, req.Params.Arguments)
		if err != nil {
			res = o.cloudFailure(&rec, err)
		} else {
			markResult(&rec, res)
		}
		if text, ok := resultText(res); ok && rec.OK && merge != nil {
			a, _ := parseArgs(req.Params.Arguments) // Meko Cloud took them
			if merged, merr := merge(ctx, a, text); merr != nil {
				o.Log.Error("couldn't add this Meko's datapacks", "tool", name, "error", merr)
				rec.OK, rec.ErrorCode = false, failCode
				res = textResult(fail(failCode, genericDetail))
			} else if merged != nil {
				res = textResult(merged)
			}
		}
		rec.Ms = time.Since(start).Milliseconds()
		o.Service.RecordCall(ctx, rec)
		return res, nil
	}
}

// resultText is a result's text, unless it failed.
func resultText(res *sdk.CallToolResult) (string, bool) {
	if res == nil || res.IsError || len(res.Content) == 0 {
		return "", false
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		return "", false
	}
	return tc.Text, true
}

// forwardedCall is a call log row for a call sent on to Meko Cloud, with the ids its arguments name.
func forwardedCall(at time.Time, name, cred string, raw json.RawMessage) store.Call {
	rec := store.Call{At: at, Tool: name, Credential: cred, Location: store.LocationCloud}
	a, err := parseArgs(raw)
	if err != nil {
		return rec
	}
	rec.DatapackID, _ = a.str("datapack_id")
	rec.DatapackID = strings.TrimSpace(rec.DatapackID)
	conv, _ := a.str("conversation_id")
	rec.ConversationID = normalizeID(conv)
	rec.AgentID, _ = a.str("agent_id")
	rec.AgentID = strings.TrimSpace(rec.AgentID)
	return rec
}

// markResult marks the row with how Meko Cloud's result went: a failed call, or an error reply.
func markResult(rec *store.Call, res *sdk.CallToolResult) {
	if res.IsError {
		rec.ErrorCode = "invalid_arguments"
		return
	}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*sdk.TextContent); ok {
			var e struct {
				Error any `json:"error"`
			}
			if json.Unmarshal([]byte(tc.Text), &e) == nil {
				if code, ok := e.Error.(string); ok && code != "" {
					rec.ErrorCode = code
					return
				}
			}
		}
	}
	rec.OK = true
}

// cloudFailure is the error reply for a call that didn't reach Meko Cloud, or that it didn't answer:
// cloud_unreachable, cloud_key_refused, cloud_not_linked or cloud_call_failed, with what to do.
func (o Options) cloudFailure(rec *store.Call, err error) *sdk.CallToolResult {
	code, detail := "cloud_call_failed", genericDetail
	var ce *cloudlink.CallError
	if !errors.As(err, &ce) {
		// The link couldn't be read: the reply stays generic, and the log says why.
		o.Log.Warn("can't send a call to Meko Cloud", "tool", rec.Tool, "error", err)
	} else {
		detail = ce.Error()
		switch ce.Code {
		case cloudlink.CodeUnreachable:
			code = "cloud_unreachable"
		case cloudlink.CodeKeyRefused:
			code = "cloud_key_refused"
		case cloudlink.CodeNotLinked:
			code = "cloud_not_linked"
		}
	}
	rec.ErrorCode = code
	return textResult(fail(code, detail))
}

// textResult is a reply as /mcp sends one: its JSON as text, and as structuredContent's result.
func textResult(reply any) *sdk.CallToolResult {
	b, _ := json.Marshal(reply) //nolint:errchkjson // a toolError or a map of plain values
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
		StructuredContent: map[string]any{"result": string(b)}}
}

// cloudUploadFile is knowledgebase_upload_file while Meko Cloud is linked: it reads the file on this
// machine, asks Meko Cloud's knowledgebase_upload_document for an upload link, posts the file there,
// and answers with knowledgebase_document_status's reply plus already_uploaded.
func (o Options) cloudUploadFile(cred string) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		start := time.Now()
		rec := forwardedCall(start, uploadFileTool.name, cred, req.Params.Arguments)
		res := o.uploadToCloud(ctx, &rec, req.Params.Arguments)
		rec.Ms = time.Since(start).Milliseconds()
		o.Service.RecordCall(ctx, rec)
		return res, nil
	}
}

func (o Options) uploadToCloud(ctx context.Context, rec *store.Call, raw json.RawMessage) *sdk.CallToolResult {
	a, err := parseArgs(raw)
	if err != nil {
		rec.ErrorCode = "invalid_arguments"
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{
			Text: "Error calling tool '" + uploadFileTool.name + "': " + err.Error()}}}
	}
	refuse := func(code, detail string) *sdk.CallToolResult {
		rec.ErrorCode = code
		return textResult(fail(code, detail))
	}
	path, _ := a.str("path")
	if strings.TrimSpace(path) == "" {
		return refuse("VALIDATION_ERROR", "path is required")
	}
	// Meko reads the path as its own user, as for this Meko's datapacks (knowledgeUploadFile).
	if rec.Credential != "admin" && rec.Credential != "client" {
		return refuse("token_required", "knowledgebase_upload_file needs the client token "+
			"(meko token prints it). Use knowledgebase_upload_document instead.")
	}
	lf, err := knowledge.OpenLocal(strings.TrimSpace(path))
	if err != nil {
		if errors.Is(err, knowledge.ErrInvalid) {
			return refuse("VALIDATION_ERROR", strings.TrimPrefix(err.Error(), knowledge.ErrInvalid.Error()+": "))
		}
		return refuse(uploadFileTool.failCode, err.Error())
	}
	defer func() { _ = lf.File.Close() }()
	pass := map[string]any{}
	for _, k := range []string{"conversation_id", "datapack_id", "agent_id"} {
		if v, _ := a.str(k); v != "" {
			pass[k] = v
		}
	}
	call := func(name string, extra map[string]any) (*sdk.CallToolResult, map[string]any, error) {
		args := map[string]any{}
		for k, v := range pass {
			args[k] = v
		}
		for k, v := range extra {
			args[k] = v
		}
		b, _ := json.Marshal(args) //nolint:errchkjson // plain values
		res, ferr := o.Cloud.Forward(ctx, name, b)
		if ferr != nil {
			return o.cloudFailure(rec, ferr), nil, ferr
		}
		var reply map[string]any
		if !res.IsError && len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok {
				_ = json.Unmarshal([]byte(tc.Text), &reply)
			}
		}
		if res.IsError || reply == nil || reply["error"] != nil {
			markResult(rec, res)
			return res, nil, errors.New("refused")
		}
		return res, reply, nil
	}
	res, offer, err := call("knowledgebase_upload_document", map[string]any{"filename": lf.Name, "size": lf.Size,
		"content_hash": lf.Hash, "content_head_base64": base64.StdEncoding.EncodeToString(lf.Head)})
	if err != nil {
		return res
	}
	already, _ := offer["already_uploaded"].(bool)
	key, _ := offer["key"].(string)
	if !already {
		link, _ := offer["url"].(string)
		fields := map[string]string{}
		if f, ok := offer["fields"].(map[string]any); ok {
			for k, v := range f {
				if s, ok := v.(string); ok {
					fields[k] = s
				}
			}
		}
		if _, err = lf.File.Seek(0, io.SeekStart); err != nil {
			return refuse(uploadFileTool.failCode, err.Error())
		}
		if err = cloudlink.PostUpload(ctx, link, fields, lf.Name, lf.File); err != nil {
			return refuse(uploadFileTool.failCode, "Meko Cloud didn't take the file: "+err.Error())
		}
	}
	res, status, err := call("knowledgebase_document_status", map[string]any{"document_key": key})
	if err != nil {
		return res
	}
	status["already_uploaded"] = already
	rec.OK = true
	return textResult(status)
}
