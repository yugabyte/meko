// Package mcp is Meko's MCP server at /mcp: Meko Cloud's tools, with their names, arguments and
// reply shapes, on the official MCP Go SDK in stateless mode with JSON replies. While Meko Cloud is
// linked, it offers Meko Cloud's own tools and sends every call there (cloud.go).
//
// Tools take their arguments raw (the SDK's low-level AddTool, which validates nothing), so they
// read them as loosely as Cloud does and ignore ones they don't know. Each result is the reply's
// JSON as text, with structuredContent {"result": <that text>} as Cloud's server wraps it. Errors
// a caller can act on are replies too ({"error", "detail"}, isError false); only arguments of the
// wrong type fail the call (isError true).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
)

// MaxBody is the largest request /mcp takes: room for a 5 MiB artifact in base64.
const MaxBody = 8 << 20

// Options configure the MCP handler.
type Options struct {
	Service *memory.Service
	Version string
	Log     *slog.Logger
	// Credential is the credential a request got in with (admin, client, none, or the web
	// listener's remote or local), recorded with
	// each call. The server's auth check sets it before the handler runs.
	Credential func(*http.Request) string
	// Cloud, if set, is the linked Meko Cloud: while it's linked, /mcp offers its tools and sends
	// every call there (cloud.go).
	Cloud Cloud
}

// instructions is what initialize tells clients about Meko.
const instructions = `Meko is your memory: facts, conversations and files that last across sessions and are
shared by every agent that uses the same datapack. This server is Meko running on this machine.

Start with context_search: it searches memories, the knowledge base and past conversations at
once. knowledgebase_search searches only the knowledge base: uploaded files and promoted
memories. To add a file on this machine, pass its path to knowledgebase_upload_file; for one
that isn't, use knowledgebase_upload_document. Save a fact
with memory_add. Store a chat with conversation_create, then conversation_add_message for each
turn. Pass the same conversation_id to every call of a session.

agent_id says who is calling: <client>:<project> for a coding agent (claude_code:myrepo), a
client name otherwise. Blank means meko_agent.

datapack_id picks the datapack: Meko's separate spaces, each with its own memories, conversations
and files. Blank uses the default one. When the user hasn't said which to use, ask them (datapack_list
shows them all) before your first call, and tell them you'll use the default datapack if they don't
pick one. datapack_metrics shows how a datapack's context is doing: its size, how much context_search
saves sending, and how relevant its results are.`

// NewHandler is the /mcp handler.
func NewHandler(o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Credential == nil {
		o.Credential = func(*http.Request) string { return "" }
	}
	tools := allTools()
	servers := map[string]*sdk.Server{}
	serverFor := func(cred string) *sdk.Server {
		if s, ok := servers[cred]; ok {
			return s
		}
		s := sdk.NewServer(&sdk.Implementation{Name: "Meko", Version: o.Version},
			&sdk.ServerOptions{Instructions: instructions})
		for _, t := range tools {
			s.AddTool(t.sdkTool(), o.handler(t, cred))
		}
		servers[cred] = s
		return s
	}
	// One server per credential, so each call knows (and logs) what it got in with. Made up front:
	// the SDK calls getServer from concurrent requests.
	for _, cred := range []string{"admin", "client", "none", "remote", "local", ""} {
		serverFor(cred)
	}
	cloud := &cloudServers{}
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		cred := o.Credential(r)
		if _, ok := servers[cred]; !ok {
			cred = ""
		}
		// A link that can't be read (a Keychain that refused, say) may be a linked one: its calls
		// then fail with the reason in the log, rather than land in this Meko's datapacks.
		if o.Cloud != nil {
			if linked, err := o.Cloud.IsLinked(); err != nil || linked {
				return o.cloudServerFor(r.Context(), cloud, cred)
			}
		}
		return servers[cred]
	}, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxBody})
}

// sdkTool is how tools/list describes the tool.
func (t *tool) sdkTool() *sdk.Tool {
	f, tr := false, true
	ann := &sdk.ToolAnnotations{Title: t.title}
	switch t.kind {
	case readOnly:
		ann.ReadOnlyHint, ann.DestructiveHint = true, &f
	case write:
		ann.DestructiveHint = &f
	case destructive:
		ann.DestructiveHint = &tr
	}
	return &sdk.Tool{Name: t.name, Title: t.title, Description: t.desc, InputSchema: t.schema(), Annotations: ann}
}

// genericDetail is what a call that failed unexpectedly says; the cause is in the server's log.
const genericDetail = "Meko couldn't complete the call. The server's log says why."

// handler runs a tool and records the call.
func (o Options) handler(t *tool, cred string) sdk.ToolHandler {
	known := map[string]bool{}
	for _, p := range t.params {
		known[p.name] = true
	}
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		start := time.Now()
		c := &call{svc: o.Service, rec: store.Call{At: start, Tool: t.name, Credential: cred}}
		a, err := parseArgs(req.Params.Arguments)
		var reply any
		if err == nil {
			for name := range a {
				if !known[name] {
					o.Log.Debug("ignored an unknown argument", "tool", t.name, "argument", name)
				}
			}
			reply, err = t.run(ctx, c, a)
		}
		res := o.result(t, c, reply, err)
		c.rec.Ms = time.Since(start).Milliseconds()
		o.Service.RecordCall(ctx, c.rec)
		if s := c.search; s != nil && c.rec.OK && c.rec.AgentID != memory.WebUIAgent && !fromAgentK(req) {
			s.DatapackID, s.Tool, s.AgentID, s.ConversationID, s.At = c.rec.DatapackID, t.name, c.rec.AgentID,
				c.rec.ConversationID, start
			o.Service.RecordSearch(ctx, *s)
		}
		return res, nil
	}
}

// ClientHeader names the client a request comes from. AgentK sends ClientAgentK on its own calls (its
// search at a session's start, its learn step's lookups), which aren't questions for Context Gaps: a
// header, not a tool argument, since Meko Cloud refuses arguments it doesn't know.
const (
	ClientHeader = "X-Meko-Client"
	ClientAgentK = "agentk"
)

// fromAgentK says whether a call is AgentK's own.
func fromAgentK(req *sdk.CallToolRequest) bool {
	return req.Extra != nil && strings.EqualFold(strings.TrimSpace(req.Extra.Header.Get(ClientHeader)), ClientAgentK)
}

// result is the MCP result for a tool's reply or error, and marks the call log row.
func (o Options) result(t *tool, c *call, reply any, err error) *sdk.CallToolResult {
	var te *toolError
	var ae *argError
	var done *partial
	errors.As(err, &done)
	err = serviceError(err)
	switch {
	case err == nil:
		// A few tools reply with their error rather than returning it, and set the code.
		c.rec.OK = c.rec.ErrorCode == ""
	case errors.As(err, &te):
		c.rec.ErrorCode = te.Code
		reply = withDone(te, done)
	case errors.As(err, &ae) || isParseError(err):
		c.rec.ErrorCode = "invalid_arguments"
		return &sdk.CallToolResult{IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("Error calling tool '%s': %v", t.name, err)}}}
	default:
		o.Log.Error("tool failed", "tool", t.name, "error", err)
		c.rec.ErrorCode = t.failCode
		reply = withDone(fail(t.failCode, genericDetail), done)
	}
	if r, isText := reply.(plainText); isText {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(r)}},
			StructuredContent: map[string]any{"result": string(r)}}
	}
	b, merr := json.Marshal(reply)
	if merr != nil {
		o.Log.Error("couldn't encode a reply", "tool", t.name, "error", merr)
		b, _ = json.Marshal(fail(t.failCode, genericDetail)) //nolint:errchkjson // a fixed shape
		c.rec.OK, c.rec.ErrorCode = false, t.failCode
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
		StructuredContent: map[string]any{"result": string(b)}}
}

// withDone is te with what a partly done call did beside its error and detail.
func withDone(te *toolError, done *partial) *toolError {
	if done == nil {
		return te
	}
	out := &toolError{Code: te.Code, Detail: te.Detail, Extra: map[string]any{}}
	for k, v := range te.Extra {
		out.Extra[k] = v
	}
	for k, v := range done.done {
		out.Extra[k] = v
	}
	return out
}

// errArgsNotObject is arguments that aren't a JSON object.
var errArgsNotObject = errors.New("arguments must be an object")

func isParseError(err error) bool { return errors.Is(err, errArgsNotObject) }

// Call runs one tool in-process as /mcp does for a request that got in with cred, recording the
// call, and returns its reply's text: an error a caller can act on ({"error", "detail"}) is a reply
// too. It serves the REST routes that answer what a tool does (a datapack's search and memories).
func Call(ctx context.Context, o Options, cred, name string, args map[string]any) (string, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	for _, t := range allTools() {
		if t.name != name {
			continue
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return "", err
		}
		res, err := o.handler(t, cred)(ctx, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: name,
			Arguments: raw}})
		if err != nil {
			return "", err
		}
		text := ""
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok {
				text = tc.Text
			}
		}
		if res.IsError {
			return "", errors.New(text)
		}
		return text, nil
	}
	return "", fmt.Errorf("no tool is named %q", name)
}
