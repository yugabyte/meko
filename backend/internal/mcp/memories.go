package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

// defaultPageSize is memory_get_all's page, as Meko Cloud's memory list.
const defaultPageSize = 20

func memoryTools() []*tool {
	return []*tool{
		{
			name: "memory_add", title: "Add Memory", kind: write, failCode: "memory_add_failed",
			desc: "Save a fact as a memory, word for word. Use it when the user asks to remember something, or " +
				"for a fact found only in your own output or a tool's result.",
			params: []param{
				{name: "text", typ: "string", required: true, desc: "The fact to save."},
				pConversation, pAgent,
				{name: "run_id", typ: "string", nullable: true, desc: "Ignored; kept for clients that send it."},
				{name: "metadata", typ: "string", nullable: true, desc: "A JSON object, as a string, to keep with it."},
				{name: "messages", typ: "string", nullable: true,
					desc: "Instead of text: a JSON array of {role, content} messages, as a string. Each " +
						"non-system message becomes a memory."},
				pDatapack,
			},
			run: memoryAdd,
		},
		{
			name: "memory_search", title: "Search Memories", kind: readOnly, failCode: "search_failed",
			desc: "Search the datapack's memories by meaning, keywords and the names they mention. Memories " +
				"from every agent are searched. Weak matches (cosine below 0.2) are left out.",
			params: []param{
				{name: "query", typ: "string", required: true, desc: "What to find, in plain language."},
				pConversation, pAgent,
				{name: "limit", typ: "integer", def: 10, desc: "The most memories to return."},
				pDatapack,
				{name: "run_id", typ: "string", nullable: true,
					desc: "Only memories saved in this conversation (its id)."},
			},
			run: memorySearch,
		},
		{
			name: "memory_get_all", title: "List Memories", kind: readOnly, failCode: "list_failed",
			desc: "List the datapack's memories, newest first, 20 to a page. Set promoted to list the " +
				"memories promoted to the datapack's collective memory instead.",
			params: []param{
				pConversation, pAgent, pDatapack,
				{name: "run_id", typ: "string", nullable: true, desc: "Ignored; kept for clients that send it."},
				{name: "promoted", typ: "boolean", def: false, desc: "List promoted memories instead."},
			},
			run: memoryGetAll,
		},
		{
			name: "memory_get_by_id", title: "Get Memory", kind: readOnly, failCode: "get_failed",
			desc:   "Get one memory by its UUID.",
			params: []param{pMemoryID, pConversation, pAgent, pDatapack},
			run:    memoryGetByID,
		},
		{
			name: "memory_update", title: "Update Memory", kind: write, failCode: "update_failed",
			desc: "Replace a memory's text. agent_id must be the agent that wrote it.",
			params: []param{pMemoryID,
				{name: "text", typ: "string", required: true, desc: "The memory's new text."},
				pConversation, pAgent, pDatapack},
			run: memoryUpdate,
		},
		{
			name: "memory_delete_by_id", title: "Delete Memory", kind: destructive, failCode: "delete_failed",
			desc:   "Delete one memory for good. agent_id must be the agent that wrote it.",
			params: []param{pMemoryID, pConversation, pAgent, pDatapack},
			run:    memoryDeleteByID,
		},
		{
			name: "memory_delete_all", title: "Delete Memories", kind: destructive, failCode: "delete_failed",
			desc: "Delete every memory one agent saved in the datapack, for good; with run_id, only those " +
				"saved in that conversation.",
			params: []param{pConversation, pAgent,
				{name: "run_id", typ: "string", nullable: true,
					desc: "Only memories saved in this conversation: its 32-hex id."},
				pDatapack},
			run: memoryDeleteAll,
		},
	}
}

func memoryAdd(ctx context.Context, c *call, a args) (any, error) {
	conv, err := c.conversationID(a)
	if err != nil {
		return nil, err
	}
	text, err := a.str("text")
	if err != nil {
		return nil, err
	}
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	md, ok, err := a.metadata()
	if err != nil {
		return nil, err
	}
	if !ok {
		return plainText("Error: metadata must be a valid JSON string."), nil
	}
	var msgs []engine.Message
	if present, ok := a.jsonValue("messages", &msgs); !ok {
		return plainText("Error: messages must be a valid JSON array of {role, content} objects."), nil
	} else if !present {
		msgs = nil
	}
	if strings.TrimSpace(text) == "" && len(msgs) == 0 {
		return nil, fail("messages_missing", "Provide either text or messages so Mem0 knows what to store.")
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	added, err := c.svc.AddMemory(ctx, memory.AddMemory{DatapackID: dp, AgentID: agent, ConversationID: conv,
		Text: text, Messages: msgs, Metadata: md})
	if err != nil {
		return nil, err
	}
	if added == nil {
		added = []engine.Added{}
	}
	return map[string]any{"results": added}, nil
}

func memorySearch(ctx context.Context, c *call, a args) (any, error) {
	conv, err := a.str("conversation_id")
	if err != nil {
		return nil, err
	}
	c.rec.ConversationID = normalizeID(conv) // trace only, not required (as in Cloud)
	query, err := a.str("query")
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 10)
	if err != nil {
		return nil, err
	}
	run, err := a.str("run_id")
	if err != nil {
		return nil, err
	}
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	res, err := c.svc.SearchMemories(ctx, memory.Search{DatapackID: dp, ConversationID: normalizeID(run),
		Query: query, Limit: max(limit, 1)})
	if err != nil {
		return nil, err
	}
	res = memory.FilterRaw(res)
	c.rec.MemoryN = len(res)
	c.rec.ScoreHigh, c.rec.ScoreMid, c.rec.ScoreLow = wire.Bands(memory.ContextResults{Memory: res})
	return map[string]any{"results": res}, nil
}

func memoryGetAll(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	if _, err := c.agent(a); err != nil {
		return nil, err
	}
	promoted, err := a.boolean("promoted")
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	criteria := map[string]any{"datapack_id": dp, "user_id": c.svc.UserID()}
	if promoted {
		return map[string]any{"criteria": criteria, "promoted_memories": []any{}, "total": 0, "page": 1,
			"page_size": defaultPageSize, "total_pages": 1}, nil
	}
	ms, total, err := c.svc.ListMemoryPage(ctx, memory.Page{DatapackID: dp, Page: 1, PageSize: defaultPageSize})
	if err != nil {
		return nil, err
	}
	return wire.MemoryListOf(ms, criteria, total, 1, defaultPageSize), nil
}

// ownMemory is a memory of the datapack the call may change: one the local user wrote as agent.
// Anything else is not_found, as in Cloud, so an id says nothing about memories that aren't yours.
func ownMemory(ctx context.Context, c *call, dp, id, agent string) error {
	m, err := c.svc.GetMemory(ctx, dp, id)
	if errors.Is(err, memory.ErrNotFound) || err == nil && m.AgentID != agent {
		return fail("not_found", fmt.Sprintf("No memory found with id %s.", id))
	}
	return err
}

func memoryGetByID(ctx context.Context, c *call, a args) (any, error) {
	id, dp, _, err := memoryCall(ctx, c, a)
	if err != nil {
		return nil, err
	}
	m, err := c.svc.GetMemory(ctx, dp, id)
	if errors.Is(err, memory.ErrNotFound) {
		return nil, fail("not_found", fmt.Sprintf("No memory found with id %s.", id))
	}
	return m, err
}

// memoryCall reads what the single-memory tools share: memory_id, conversation_id, agent_id and
// the datapack.
func memoryCall(ctx context.Context, c *call, a args) (id, dp, agent string, err error) {
	if _, err = c.conversationID(a); err != nil {
		return
	}
	if id, err = memoryID(a); err != nil {
		return
	}
	if agent, err = c.agent(a); err != nil {
		return
	}
	dp, err = c.datapack(ctx, a)
	return
}

func memoryUpdate(ctx context.Context, c *call, a args) (any, error) {
	id, dp, agent, err := memoryCall(ctx, c, a)
	if err != nil {
		return nil, err
	}
	text, err := a.str("text")
	if err != nil {
		return nil, err
	}
	if err = ownMemory(ctx, c, dp, id, agent); err != nil {
		return nil, err
	}
	if err = c.svc.UpdateMemory(ctx, dp, id, text); err != nil {
		return nil, err
	}
	return map[string]string{"message": "Memory updated successfully!"}, nil
}

func memoryDeleteByID(ctx context.Context, c *call, a args) (any, error) {
	id, dp, agent, err := memoryCall(ctx, c, a)
	if err != nil {
		return nil, err
	}
	if err = ownMemory(ctx, c, dp, id, agent); err != nil {
		return nil, err
	}
	if err = c.svc.DeleteMemory(ctx, dp, id); err != nil {
		return nil, err
	}
	return map[string]string{"message": "Memory deleted successfully!"}, nil
}

func memoryDeleteAll(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	run, err := a.str("run_id")
	if err != nil {
		return nil, err
	}
	run = normalizeID(run)
	if a.present("run_id") && run != "" && !isHex32(run) {
		return nil, fail("invalid_run_id", "run_id must be a conversation id: 32 lowercase hex chars, hyphens "+
			"optional. Omit run_id to delete them all deliberately.")
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	if _, err = c.svc.DeleteAllMemories(ctx, dp, agent, run); err != nil {
		return nil, err
	}
	return map[string]string{"message": "Memories deleted successfully!"}, nil
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
