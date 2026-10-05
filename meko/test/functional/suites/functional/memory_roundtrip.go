package functional

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

const extractionWithin = 2 * time.Minute

func init() {
	core.Register(core.Test{
		Name:       "functional/memory_roundtrip",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "memory_add stores a text and embeds it, with no chat call, and memory_search finds it and embeds the query. " +
			"A conversation turn makes mem0 ask the model which memories to keep. The scripted answer is what gets stored, and memory_search finds it.",
		Run: memoryRoundtrip,
	})
}

func memoryRoundtrip(t *core.T) error {
	s, err := newMemStack(t)
	defer s.close()
	if err != nil {
		return err
	}
	ctx := context.Background()
	marker := marker(t)

	if err := t.Step("conversation_create", func(h *core.StepHandle) error {
		if err := s.createConversation(); err != nil {
			return err
		}
		h.Set("conversation_id", s.conversion)
		return nil
	}); err != nil {
		return err
	}

	text := fmt.Sprintf("My favourite tea is jasmine green tea, code %s.", marker)
	if err := t.Step("memory_add_embeds_the_text", func(h *core.StepHandle) error {
		if _, terr := s.tool("memory_add", map[string]any{"text": text, "conversation_id": s.conversion}); terr != nil {
			return terr
		}
		calls, err := waitCall(t, s.llm, llmmock.RouteEmbeddings, marker, 1, 30*time.Second)
		if err != nil {
			return err
		}
		h.Set("embedding_calls", len(calls))
		h.Set("embedding_model", calls[0].Model)
		if calls[0].Status != 200 {
			return fmt.Errorf("the embedding call got status %d", calls[0].Status)
		}
		if chat, err := chatCalls(s.llm, marker); err != nil || len(chat) != 0 {
			return fmt.Errorf("memory_add asked the chat model %d time(s) (%v), it stores the text as given", len(chat), err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("memory_search_finds_it", func(h *core.StepHandle) error {
		before, _ := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings})
		after := 0
		if n := len(before); n > 0 {
			after = before[n-1].Seq
		}
		query := "jasmine tea " + marker
		out, terr := s.tool("memory_search", map[string]any{"query": query, "conversation_id": s.conversion})
		if terr != nil {
			return terr
		}
		if !strings.Contains(out, marker) {
			return fmt.Errorf("memory_search for %q did not return the stored memory: %s", query, out)
		}
		calls, err := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings, Contains: query, After: after})
		if err != nil {
			return err
		}
		if len(calls) == 0 {
			return fmt.Errorf("the mock never saw the search query %q embedded", query)
		}
		return nil
	}); err != nil {
		return err
	}

	extracted := "dogname" + marker
	return t.Step("conversation_turn_extracts_the_scripted_memory", func(h *core.StepHandle) error {
		fact := fmt.Sprintf("The user's dog is called %s.", extracted)
		id, err := s.llm.AddScript(ctx, llmmock.Script{
			Match: extracted,
			Reply: fmt.Sprintf(`{"memory": [{"id": "0", "text": %q}]}`, fact),
		})
		if err != nil {
			return err
		}
		defer s.llm.RemoveScript(context.Background(), id)
		if _, terr := s.tool("conversation_add_message", map[string]any{
			"conversation_id": s.conversion,
			"input":           "Please remember that my dog is called " + extracted + ".",
			"output":          "Noted.",
		}); terr != nil {
			return terr
		}
		calls, err := waitCall(t, s.llm, llmmock.RouteChat, extracted, 1, extractionWithin)
		if err != nil {
			return err
		}
		h.Set("extraction_answer", calls[0].Answer)
		if calls[0].Answer != "script:"+id {
			return fmt.Errorf("the extraction call was answered by %q, want the script %s", calls[0].Answer, "script:"+id)
		}
		return t.WaitFor("memory_search to find the extracted memory", extractionWithin, mockPoll, func() (bool, error) {
			out, terr := s.tool("memory_search", map[string]any{"query": "dog " + extracted, "conversation_id": s.conversion})
			if terr != nil {
				return false, terr
			}
			return strings.Contains(out, fact), nil
		})
	})
}

// marker is a word no other test uses, so the mock's recorded calls can be
// filtered to this test's own on a stack the tests share.
func marker(t *core.T) string {
	return "mq" + strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, t.ResourceSuffix()))
}
