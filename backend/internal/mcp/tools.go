package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/memoryengine"
	"github.com/yugabyte/meko/backend/internal/store"
)

// kind is a tool's annotation preset, as Meko Cloud's.
type kind int

const (
	readOnly kind = iota
	write
	destructive
)

// param is one argument a tool advertises in its input schema.
type param struct {
	name, typ, desc string // typ: string, integer, boolean or array (of strings)
	required        bool
	nullable        bool // anyOf [type, null], default null, as Cloud advertises optional strings
	hidden          bool // read, but left out of the input schema (Cloud's internal arguments)
	def             any
}

// tool is one MCP tool: what tools/list says about it and what tools/call runs.
type tool struct {
	name, title, desc string
	kind              kind
	params            []param
	// failCode is the error code a call reports when something unexpected fails.
	failCode string
	run      func(ctx context.Context, c *call, a args) (any, error)
}

// call is one tools/call in progress: the service, and the call log row it fills in.
type call struct {
	svc *memory.Service
	rec store.Call
}

// toolError is an error a tool returns in its result, as Meko Cloud does: {"error", "detail"},
// with isError false.
type toolError struct {
	Code   string         `json:"error"`
	Detail string         `json:"detail,omitempty"`
	Extra  map[string]any `json:"-"`
}

func (e *toolError) Error() string { return e.Code + ": " + e.Detail }

// MarshalJSON writes the error with its extra keys beside error and detail.
func (e *toolError) MarshalJSON() ([]byte, error) {
	m := map[string]any{"error": e.Code}
	if e.Detail != "" {
		m["detail"] = e.Detail
	}
	for k, v := range e.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

func fail(code, detail string) *toolError { return &toolError{Code: code, Detail: detail} }

// partial is an error that stopped a call after part of its work was done; done says what was,
// and goes in the error reply beside error and detail.
type partial struct {
	err  error
	done map[string]any
}

func (p *partial) Error() string { return p.err.Error() }
func (p *partial) Unwrap() error { return p.err }

// plainText is a reply Cloud sends as plain text rather than JSON, kept for its few legacy paths.
type plainText string

// structured is a reply whose structuredContent is the object itself (track_token_usage's),
// rather than its JSON text.
type structured struct{ v any }

// normalizeID is a conversation id as callers may write it: hyphens dropped, trimmed, lowercase.
func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(id, "-", "")))
}

// conversationID is the call's conversation_id, which most tools require (Cloud nests their
// traces under it).
func (c *call) conversationID(a args) (string, error) {
	raw, err := a.str("conversation_id")
	if err != nil {
		return "", err
	}
	id := normalizeID(raw)
	if id == "" {
		return "", fail("conversation_id_required",
			"Pass conversation_id from the stored chat (conversation_create / add_message).")
	}
	c.rec.ConversationID = id
	return id, nil
}

// agent is the call's agent_id, meko_agent when blank.
func (c *call) agent(a args) (string, error) {
	s, err := a.str("agent_id")
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = memory.DefaultAgentID
	}
	c.rec.AgentID = s
	return s, nil
}

// datapack is the datapack the call works in: the one it names, or the default.
func (c *call) datapack(ctx context.Context, a args) (string, error) {
	named, err := a.str("datapack_id")
	if err != nil {
		return "", err
	}
	named = strings.TrimSpace(named)
	id, err := c.svc.ResolveDatapack(ctx, named)
	switch {
	case errors.Is(err, memory.ErrNoDefault):
		return "", fail("datapack_id_required", "Pass datapack_id (UUID). Use datapack_search(query=...) to find it.")
	case errors.Is(err, memory.ErrNoDatapack):
		return "", fail("datapack_access_denied", fmt.Sprintf("no datapack %s", named))
	case err != nil:
		return "", err
	}
	c.rec.DatapackID = id
	return id, nil
}

// memoryID is the call's memory_id, which must be a UUID.
func memoryID(a args) (string, error) {
	id, err := a.str("memory_id")
	if err != nil {
		return "", err
	}
	if _, perr := uuid.Parse(id); perr != nil || len(id) != 36 {
		return "", fail("invalid_memory_id", fmt.Sprintf("'%s' is not a valid memory ID (UUID).  Use the UUID from "+
			"the 'id' field in memory_search or memory_get_all results.", id))
	}
	return id, nil
}

// serviceError turns the service's errors into the tool errors Meko Cloud returns for them; any
// other error is left for the caller to report as a failure.
func serviceError(err error) error {
	var notReady *memoryengine.NotReadyError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, memory.ErrNoDatapack):
		return fail("datapack_access_denied", err.Error())
	case errors.Is(err, memory.ErrAgentMismatch):
		return fail("agent_id_mismatch", "The conversation belongs to another agent. Use the agent_id that created it.")
	case errors.Is(err, memory.ErrNoConversation):
		return fail("not_found", "Conversation not found")
	case errors.Is(err, memoryengine.ErrUnavailable), errors.As(err, &notReady):
		return fail("memory_engine_unavailable", err.Error())
	case errors.Is(err, memory.ErrBadRequest):
		return fail("bad_request", strings.TrimPrefix(err.Error(), memory.ErrBadRequest.Error()+": "))
	}
	return err
}

// schema is a tool's input schema from its params.
func (t *tool) schema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.params {
		if p.hidden {
			continue
		}
		var s map[string]any
		switch {
		case p.typ == "array":
			s = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		case p.nullable:
			s = map[string]any{"anyOf": []any{map[string]any{"type": p.typ}, map[string]any{"type": "null"}},
				"default": nil}
		default:
			s = map[string]any{"type": p.typ}
		}
		if p.def != nil {
			s["default"] = p.def
		}
		if p.desc != "" {
			s["description"] = p.desc
		}
		props[p.name] = s
		if p.required {
			required = append(required, p.name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// Parameters many tools share.
var (
	pConversation = param{name: "conversation_id", typ: "string", required: true,
		desc: "The conversation this call belongs to: the id conversation_create returned."}
	pAgent = param{name: "agent_id", typ: "string", nullable: true,
		desc: "Who is calling: <client>:<project> for a coding agent (claude_code:myrepo), a client name " +
			"otherwise. Blank is meko_agent."}
	pDatapack = param{name: "datapack_id", typ: "string", nullable: true,
		desc: "The datapack (UUID). Blank uses the default datapack."}
	pMemoryID = param{name: "memory_id", typ: "string", required: true,
		desc: "The memory's UUID, from the id field of memory_search or memory_get_all."}
)

// ToolInfo is a tool as `meko list mcp-tools` shows it.
type ToolInfo struct {
	Name, Title, Summary string
}

// Tools lists every tool in tools/list's order, each with the first sentence of its description.
func Tools() []ToolInfo {
	ts := allTools()
	out := make([]ToolInfo, len(ts))
	for i, t := range ts {
		out[i] = ToolInfo{Name: t.name, Title: t.title, Summary: firstSentence(t.desc)}
	}
	return out
}

// firstSentence is text up to its first ". " (or all of it), without the full stop.
func firstSentence(text string) string {
	if i := strings.Index(text, ". "); i >= 0 {
		return text[:i]
	}
	return strings.TrimSuffix(text, ".")
}

// allTools is every tool, in the order tools/list gives them.
func allTools() []*tool {
	out := make([]*tool, 0, 32)
	out = append(out, searchTools()...)
	out = append(out, knowledgeTools()...)
	out = append(out, memoryTools()...)
	out = append(out, conversationTools()...)
	out = append(out, datapackTools()...)
	out = append(out, artifactTools()...)
	out = append(out, usageTools()...)
	return out
}
