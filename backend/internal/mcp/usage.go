package mcp

import (
	"context"
	"errors"
)

func usageTools() []*tool {
	return []*tool{{
		name: "track_token_usage", title: "Track Token Usage", kind: write, failCode: "track_failed",
		desc: "Record tokens an LLM or embedding call spent for a conversation, so Meko's usage reflects " +
			"them. Only the counts are kept.",
		params: []param{
			{name: "conversation_id", typ: "string", required: true, desc: "The conversation the tokens were spent for."},
			{name: "name", typ: "string", required: true, desc: "What spent them, such as \"LLM Response\"."},
			{name: "input_tokens", typ: "integer", def: 0, desc: "Prompt tokens."},
			{name: "output_tokens", typ: "integer", def: 0, desc: "Completion tokens; 0 for embeddings."},
			{name: "total_tokens", typ: "integer", nullable: true, desc: "Input plus output, when not given."},
			{name: "model", typ: "string", nullable: true, desc: "The model."},
			{name: "message_id", typ: "string", nullable: true, desc: "The turn they were spent for."},
			pDatapack,
		},
		run: trackTokenUsage,
	}}
}

// trackTokenUsage replies as Cloud's does: an object, not its JSON text, in structuredContent, and
// {"recorded": false, "error"} rather than an error result.
func trackTokenUsage(ctx context.Context, c *call, a args) (any, error) {
	notRecorded := func(why string) (any, error) {
		c.rec.ErrorCode = why
		return structured{map[string]any{"recorded": false, "error": why}}, nil
	}
	if _, err := c.conversationID(a); err != nil {
		var te *toolError
		if errors.As(err, &te) {
			return notRecorded(te.Code)
		}
		return nil, err
	}
	in, err := a.integer("input_tokens", 0)
	if err != nil {
		return nil, err
	}
	out, err := a.integer("output_tokens", 0)
	if err != nil {
		return nil, err
	}
	if in < 0 || out < 0 {
		return notRecorded("token counts must be non-negative")
	}
	if _, err = c.datapack(ctx, a); err != nil {
		return nil, err
	}
	c.rec.InputTokens, c.rec.OutputTokens = in, out
	return structured{map[string]any{"recorded": true}}, nil
}
