package rest

import (
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/mcp"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/wire"
)

// Conversation page sizes, as Meko Cloud's.
const (
	defaultConversations = 20
	maxConversations     = 200
	defaultTurns         = 100
	maxTurns             = 500
)

// conversationSummary is a conversation as meko_ui's list shows it. Meko records no LLM calls
// for a turn, so llm_calls is 0.
type conversationSummary struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	Title          string `json:"title"`
	StartedAt      string `json:"started_at"`
	LastActivityAt string `json:"last_activity_at"`
	FirstMessage   string `json:"first_message"`
	Messages       int    `json:"messages"`
	MemoriesAdded  int    `json:"memories_added"`
	LLMCalls       int    `json:"llm_calls"`
	UserID         string `json:"user_id"`
	UserEmailID    string `json:"user_email_id,omitempty"`
}

func (a *api) summary(cs store.ConversationSummary, email string) conversationSummary {
	return conversationSummary{ID: cs.ID, AgentID: cs.AgentID, Title: cs.Title, StartedAt: wire.Time(cs.CreatedAt),
		LastActivityAt: wire.Time(cs.LastActivityAt), FirstMessage: cs.FirstMessage, Messages: cs.MessageCount,
		MemoriesAdded: cs.MemoriesAdded, UserID: a.Service.UserID(), UserEmailID: email}
}

func (a *api) listConversations(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	limit, ok, err := queryInt(c, "limit", defaultConversations)
	if !ok {
		return err
	}
	offset, ok, err := queryInt(c, "offset", 0)
	if !ok {
		return err
	}
	limit, offset = min(max(limit, 1), maxConversations), max(offset, 0)
	d, err := a.Service.Datapack(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	convs, more, err := a.Service.ConversationSummaries(ctx(c), dp, c.QueryParam("q"), limit, offset)
	if err != nil {
		return serviceError(c, err)
	}
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return err
	}
	out := make([]conversationSummary, len(convs))
	for i, cs := range convs {
		out[i] = a.summary(cs, p.Email)
	}
	return envelope.OK(c, map[string]any{"datapack_id": dp, "datapack_name": d.Name, "langfuse_project_id": nil,
		"limit": limit, "offset": offset, "has_more": more, "conversations": out})
}

// turn is one turn of a conversation's thread, with the memories it produced (still memories, and
// promoted since).
type turn struct {
	MessageID           string   `json:"message_id"`
	ObservationID       string   `json:"observation_id"`
	StartedAt           string   `json:"started_at"`
	Input               string   `json:"input"`
	Output              string   `json:"output"`
	Reasoning           string   `json:"reasoning,omitempty"`
	Plan                []string `json:"plan,omitempty"`
	LatencyMs           int      `json:"latency_ms"`
	MemoriesExtracted   int      `json:"memories_extracted"`
	MemoryIDs           []string `json:"memory_ids"`
	MemoryTexts         []string `json:"memory_texts"`
	PromotedMemoryIDs   []string `json:"promoted_memory_ids"`
	PromotedMemoryTexts []string `json:"promoted_memory_texts"`
	LLMCalls            int      `json:"llm_calls"`
	TotalTokens         int      `json:"total_tokens"`
	// Calls is Meko's own decision trace for the turn: what it read and what it wrote, shaped as
	// /usage's recent calls.
	Calls []map[string]any `json:"calls"`
}

// callsOf is calls from the call log as /usage and the conversation thread give them.
func callsOf(cs []store.Call) []map[string]any {
	out := make([]map[string]any, len(cs))
	for i, r := range cs {
		out[i] = map[string]any{"id": r.ID, "steps": r.Steps, "at": wire.Time(r.At), "tool": r.Tool,
			"agent_id": r.AgentID, "ok": r.OK, "error_code": r.ErrorCode, "ms": r.Ms, "memory_n": r.MemoryN,
			"kb_n": r.KBN, "conversation_n": r.ConversationN, "read_only": mcp.ReadOnly(r.Tool)}
	}
	return out
}

func turnOf(t memory.Turn) turn {
	out := turn{MessageID: t.ID, ObservationID: t.ID, StartedAt: wire.Time(t.CreatedAt), Input: t.Input,
		Output: t.Output, Reasoning: t.Reasoning, Plan: t.Plan, MemoryIDs: []string{}, MemoryTexts: []string{},
		PromotedMemoryIDs: []string{}, PromotedMemoryTexts: []string{}, Calls: callsOf(t.Calls)}
	for _, m := range t.Memories {
		if m.Promoted {
			out.PromotedMemoryIDs = append(out.PromotedMemoryIDs, m.ID)
			out.PromotedMemoryTexts = append(out.PromotedMemoryTexts, m.Text)
		} else {
			out.MemoryIDs = append(out.MemoryIDs, m.ID)
			out.MemoryTexts = append(out.MemoryTexts, m.Text)
		}
	}
	out.MemoriesExtracted = len(out.MemoryIDs)
	return out
}

// conversation is one conversation with a page of its thread, oldest first.
func (a *api) conversation(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	limit, ok, err := queryInt(c, "messages_limit", defaultTurns)
	if !ok {
		return err
	}
	offset, ok, err := queryInt(c, "messages_offset", 0)
	if !ok {
		return err
	}
	limit, offset = min(max(limit, 1), maxTurns), max(offset, 0)
	d, err := a.Service.Datapack(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	// One turn past the page says whether there's another page.
	cs, turns, err := a.Service.ConversationThread(ctx(c), dp, c.Param("conversation"), limit+1, offset)
	if err != nil {
		return serviceError(c, err)
	}
	hasMore := len(turns) > limit
	turns = turns[:min(len(turns), limit)]
	thread := make([]turn, len(turns))
	for i, t := range turns {
		thread[i] = turnOf(t)
	}
	return envelope.OK(c, map[string]any{"datapack_id": dp, "datapack_name": d.Name, "langfuse_project_id": nil,
		"conversation": struct {
			conversationSummary
			Thread        []turn `json:"thread"`
			ThreadLimit   int    `json:"thread_limit"`
			ThreadOffset  int    `json:"thread_offset"`
			ThreadHasMore bool   `json:"thread_has_more"`
			CanIndex      bool   `json:"can_index"`
		}{a.summary(cs, ""), thread, limit, offset, hasMore, false}})
}
