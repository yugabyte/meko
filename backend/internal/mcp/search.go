package mcp

import (
	"context"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

func searchTools() []*tool {
	return []*tool{{
		name: "context_search", title: "Context Search", kind: readOnly, failCode: "search_failed",
		desc: "Find what Meko knows about something: searches memories and past conversations at once and " +
			"returns each source's results in its own bucket. Start here for any \"what do we know about...\" " +
			"question. A bucket is null when it wasn't searched, empty when nothing matched. Knowledge files " +
			"aren't searchable in this version, so knowledge_base is always null.",
		params: []param{
			{name: "query", typ: "string", required: true, desc: "What to find, in plain language."},
			pConversation, pAgent, pDatapack,
			{name: "limit", typ: "integer", def: 10, desc: "The most results per source."},
			{name: "force_source", typ: "string", def: "auto",
				desc: "auto searches every source; conversation, memory or knowledge_base searches only that one."},
		},
		run: contextSearch,
	}}
}

func contextSearch(ctx context.Context, c *call, a args) (any, error) {
	conv, err := c.conversationID(a)
	if err != nil {
		return nil, err
	}
	query, err := a.str("query")
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 10)
	if err != nil {
		return nil, err
	}
	source, err := a.str("force_source")
	if err != nil {
		return nil, err
	}
	if source == "" {
		source = "auto"
	}
	r := memory.ContextSearch{ConversationID: conv, Query: query, Limit: max(limit, 1)}
	switch source {
	case "auto":
		r.Memory, r.Conversation, r.KnowledgeBase = true, true, true
	case "memory":
		r.Memory, r.MinRawSimilarity = true, memory.MinRawSimilarity
	case "conversation":
		r.Conversation = true
	case "knowledge_base":
		r.KnowledgeBase = true
	default:
		return nil, fail("invalid_force_source", "force_source must be one of: auto, conversation, memory, knowledge_base.")
	}
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	if r.DatapackID, err = c.datapack(ctx, a); err != nil {
		return nil, err
	}
	res, err := c.svc.ContextSearch(ctx, r)
	if err != nil {
		return nil, err
	}
	reply := wire.ContextSearchOf(res)
	c.rec.MemoryN = len(res.Memory)
	if res.Conversation != nil {
		c.rec.ConversationN = len(res.Conversation.Hits)
	}
	c.rec.ScoreHigh, c.rec.ScoreMid, c.rec.ScoreLow = wire.Bands(res)
	c.rec.TokensSent, c.rec.TokensSaved = reply.TokenSavings.Sent(), reply.TokenSavings.Saved()
	c.rec.Steps = res.Steps
	return reply, nil
}
