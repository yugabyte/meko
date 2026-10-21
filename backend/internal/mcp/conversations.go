package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

// maxConversationPage is the most conversations conversation_list returns at once, as in Cloud.
const maxConversationPage = 200

func conversationTools() []*tool {
	pConvTarget := param{name: "conversation_id", typ: "string", required: true, desc: "The conversation's id."}
	return []*tool{
		{
			name: "conversation_create", title: "Create Conversation", kind: write, failCode: "create_failed",
			desc: "Start a conversation to store a chat in, then add each turn with conversation_add_message. " +
				"The same session_id always gives the same conversation, so calling this again for a session " +
				"reconnects it.",
			params: []param{pAgent,
				{name: "run_id", typ: "string", nullable: true, desc: "Kept with the conversation."},
				{name: "title", typ: "string", nullable: true, desc: "A title for people to recognize it by."},
				{name: "metadata", typ: "string", nullable: true, desc: "A JSON object, as a string, to keep with it."},
				{name: "session_id", typ: "string", def: "",
					desc: "Your own stable id for the session: the same one reconnects the same conversation."},
				pDatapack,
			},
			run: conversationCreate,
		},
		{
			name: "conversation_add_message", title: "Add Message", kind: write, failCode: "add_failed",
			desc: "Add one turn to a conversation: what the user said (input) and what you answered (output). " +
				"A turn with the same seed is stored once.",
			params: []param{pConvTarget,
				{name: "input", typ: "string", required: true, desc: "The user's prompt."},
				{name: "output", typ: "string", required: true, desc: "Your answer."},
				pAgent,
				{name: "reasoning", typ: "string", nullable: true, desc: "A sentence or three on why you answered so."},
				{name: "plan", typ: "array", desc: "The 2-4 steps you took, one sentence each."},
				{name: "metadata", typ: "string", nullable: true, desc: "A JSON object, as a string, to keep with it."},
				{name: "seed", typ: "string", nullable: true,
					desc: "A stable string for the turn: the same seed is the same turn."},
				pDatapack,
			},
			run: conversationAddMessage,
		},
		{
			name: "conversation_get", title: "Get Conversation", kind: readOnly, failCode: "get_failed",
			desc: "Get a conversation, and with include_messages its turns, oldest first.",
			params: []param{pConvTarget, pAgent,
				{name: "include_messages", typ: "boolean", def: false, desc: "Include its turns."},
				{name: "limit", typ: "integer", def: 100, desc: "The most turns to return."},
				{name: "offset", typ: "integer", def: 0, desc: "Turns to skip."},
				pDatapack},
			run: conversationGet,
		},
		{
			name: "conversation_list", title: "List Conversations", kind: readOnly, failCode: "list_failed",
			desc: "List a datapack's conversations, most recently updated first.",
			params: []param{pAgent,
				{name: "limit", typ: "integer", def: 20, desc: "The most conversations to return (200 at most)."},
				{name: "offset", typ: "integer", def: 0, desc: "Conversations to skip."},
				{name: "conversation_id", typ: "string", def: "", desc: "The conversation this call belongs to, if any."},
				pDatapack},
			run: conversationList,
		},
		{
			name: "conversation_update", title: "Update Conversation", kind: write, failCode: "update_failed",
			desc: "Change a conversation's title or metadata. Only the agent that created it can.",
			params: []param{pConvTarget, pAgent,
				{name: "title", typ: "string", nullable: true, desc: "The new title."},
				{name: "metadata", typ: "string", nullable: true, desc: "New metadata: a JSON object, as a string."},
				pDatapack},
			run: conversationUpdate,
		},
		{
			name: "conversation_delete", title: "Delete Conversation", kind: destructive, failCode: "delete_failed",
			desc:   "Delete a conversation and its turns for good. Only the agent that created it can.",
			params: []param{pConvTarget, pAgent, pDatapack},
			run:    conversationDelete,
		},
	}
}

func conversationCreate(ctx context.Context, c *call, a args) (any, error) {
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	title, err := a.optStr("title")
	if err != nil {
		return nil, err
	}
	if title != nil && strings.TrimSpace(*title) == "" {
		return nil, fail("invalid_title", "title must not be empty")
	}
	md, ok, err := a.metadata()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail("invalid_metadata", "metadata must be valid JSON")
	}
	session, err := a.str("session_id")
	if err != nil {
		return nil, err
	}
	run, err := a.str("run_id")
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	t := "Untitled conversation"
	if title != nil {
		t = *title
	}
	conv, err := c.svc.CreateConversation(ctx, memory.CreateConversation{AgentID: agent, DatapackID: dp,
		SessionID: session, RunID: run, Title: t, Metadata: md})
	if err != nil {
		return nil, err
	}
	c.rec.ConversationID = conv.ID
	return wire.ConversationOf(conv, c.svc.UserID(), false), nil
}

// target is the conversation a tool acts on: its conversation_id, the agent and the datapack.
func target(ctx context.Context, c *call, a args) (id, agent, dp string, err error) {
	raw, err := a.str("conversation_id")
	if err != nil {
		return
	}
	id = normalizeID(raw)
	c.rec.ConversationID = id
	if agent, err = c.agent(a); err != nil {
		return
	}
	dp, err = c.datapack(ctx, a)
	return
}

// conversationError is a conversation lookup's error as Cloud reports it.
func conversationError(err error, id, agent string) error {
	switch {
	case errors.Is(err, memory.ErrNoConversation):
		return fail("not_found", fmt.Sprintf("Conversation %s not found", id))
	case errors.Is(err, memory.ErrAgentMismatch):
		return fail("agent_id_mismatch", fmt.Sprintf("Conversation %s belongs to another agent, not '%s'. Use the "+
			"agent_id that created the conversation.", id, agent))
	}
	return err
}

func conversationAddMessage(ctx context.Context, c *call, a args) (any, error) {
	md, ok, err := a.metadata()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail("invalid_metadata", "metadata must be valid JSON")
	}
	id, agent, dp, err := target(ctx, c, a)
	if err != nil {
		return nil, err
	}
	r := memory.AddMessage{ConversationID: id, AgentID: agent, DatapackID: dp, Metadata: md}
	for name, dst := range map[string]*string{"input": &r.Input, "output": &r.Output, "reasoning": &r.Reasoning,
		"seed": &r.Seed} {
		if *dst, err = a.str(name); err != nil {
			return nil, err
		}
	}
	if r.Plan, err = a.strings("plan"); err != nil {
		return nil, err
	}
	if r.SkipSearchEmbed, err = a.boolean("skip_search_embed"); err != nil {
		return nil, err
	}
	added, err := c.svc.AddMessage(ctx, r)
	if err != nil {
		return nil, conversationError(err, id, agent)
	}
	return added, nil
}

func conversationGet(ctx context.Context, c *call, a args) (any, error) {
	id, agent, dp, err := target(ctx, c, a)
	if err != nil {
		return nil, err
	}
	with, err := a.boolean("include_messages")
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 100)
	if err != nil {
		return nil, err
	}
	offset, err := a.integer("offset", 0)
	if err != nil {
		return nil, err
	}
	conv, msgs, err := c.svc.GetConversation(ctx, dp, id, agent, with, limit, offset)
	if err != nil {
		return nil, conversationError(err, id, agent)
	}
	out := wire.ConversationOf(conv, c.svc.UserID(), true)
	if with {
		out.Messages = make([]wire.Message, len(msgs))
		for i, m := range msgs {
			out.Messages[i] = wire.MessageOf(m)
		}
	}
	return out, nil
}

func conversationList(ctx context.Context, c *call, a args) (any, error) {
	conv, err := a.str("conversation_id")
	if err != nil {
		return nil, err
	}
	c.rec.ConversationID = normalizeID(conv)
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 20)
	if err != nil {
		return nil, err
	}
	offset, err := a.integer("offset", 0)
	if err != nil {
		return nil, err
	}
	limit, offset = min(max(limit, 1), maxConversationPage), max(offset, 0)
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	d, err := c.svc.Datapack(ctx, dp)
	if err != nil {
		return nil, err
	}
	convs, more, err := c.svc.ListConversations(ctx, dp, "", limit, offset)
	if err != nil {
		return nil, err
	}
	out := wire.ConversationList{DatapackID: dp, DatapackName: d.Name, Limit: limit, Offset: offset, HasMore: more,
		Conversations: make([]wire.Conversation, len(convs))}
	for i, conv := range convs {
		out.Conversations[i] = wire.ConversationOf(conv, c.svc.UserID(), true)
	}
	return out, nil
}

func conversationUpdate(ctx context.Context, c *call, a args) (any, error) {
	title, err := a.optStr("title")
	if err != nil {
		return nil, err
	}
	if title != nil && strings.TrimSpace(*title) == "" {
		return nil, fail("invalid_title", "title must not be empty")
	}
	md, ok, err := a.metadata()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail("invalid_metadata", "metadata must be valid JSON")
	}
	if title == nil && md == nil {
		return nil, fail("no_fields", "Provide at least one of title or metadata to update")
	}
	id, agent, dp, err := target(ctx, c, a)
	if err != nil {
		return nil, err
	}
	conv, err := c.svc.UpdateConversation(ctx, dp, id, agent, title, md)
	if err != nil {
		return nil, conversationError(err, id, agent)
	}
	return wire.ConversationOf(conv, c.svc.UserID(), false), nil
}

func conversationDelete(ctx context.Context, c *call, a args) (any, error) {
	id, agent, dp, err := target(ctx, c, a)
	if err != nil {
		return nil, err
	}
	if err = c.svc.DeleteConversation(ctx, dp, id, agent); err != nil {
		return nil, conversationError(err, id, agent)
	}
	return map[string]string{"status": "deleted", "conversation_id": id}, nil
}
