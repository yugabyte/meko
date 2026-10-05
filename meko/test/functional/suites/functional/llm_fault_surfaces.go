package functional

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/llm_fault_surfaces",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack, FaultsSharedInfra: true},
		// It makes the LLM fail on purpose, and the services log what they get.
		Tolerates: []string{core.GuardCrashLog, core.GuardHTTP5xx},
		Timeout:   15 * time.Minute,
		Description: "The LLM mock fails the embeddings of one memory_add with a 500 and then with malformed JSON. " +
			"memory_add must answer an error the caller can read, name no internal endpoint, and store nothing. " +
			"A single 429 with Retry-After is retried and the add succeeds. After the faults stop memory_add works again. " +
			"A 500 on the extraction call is retried by the client.",
		Run: llmFaultSurfaces,
	})
}

func llmFaultSurfaces(t *core.T) error {
	s, err := newMemStack(t)
	defer s.close()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := t.Step("conversation_create", func(*core.StepHandle) error { return s.createConversation() }); err != nil {
		return err
	}

	for _, tc := range []struct {
		name  string
		fault llmmock.Fault
	}{
		{"embeddings_500", llmmock.HTTPStatus(500)},
		{"embeddings_malformed", llmmock.Malformed()},
	} {
		m := marker(t) + strings.ReplaceAll(tc.name, "_", "")
		if err := t.Step("memory_add_fails_visibly_on_"+tc.name, func(h *core.StepHandle) error {
			f := tc.fault
			f.Route, f.Match = llmmock.RouteEmbeddings, m
			fid, err := s.llm.AddFault(ctx, f)
			if err != nil {
				return err
			}
			text := "a memory that must not be stored " + m
			_, terr := s.tool("memory_add", map[string]any{"text": text, "conversation_id": s.conversion})
			_ = s.llm.RemoveFault(ctx, fid)
			if terr == nil {
				return fmt.Errorf("memory_add answered success while every embedding call failed with %s", tc.name)
			}
			h.Set("error", truncate(terr.Error(), 300))
			calls, _ := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings, Contains: m})
			h.Set("embedding_attempts", len(calls))
			out, terr2 := s.tool("memory_search", map[string]any{"query": m, "conversation_id": s.conversion})
			if terr2 != nil {
				return fmt.Errorf("memory_search after the fault ended: %w", terr2)
			}
			if strings.Contains(out, text) {
				return fmt.Errorf("memory_add reported %q but the memory was stored: %s", terr, out)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if err := t.Step("memory_add_retries_a_429", func(h *core.StepHandle) error {
		m := marker(t) + "rate"
		f := llmmock.RateLimit(time.Second)
		f.Route, f.Match, f.Times = llmmock.RouteEmbeddings, m, 1
		fid, err := s.llm.AddFault(ctx, f)
		if err != nil {
			return err
		}
		defer s.llm.RemoveFault(context.Background(), fid)
		if _, terr := s.tool("memory_add", map[string]any{"text": "retried after a rate limit " + m, "conversation_id": s.conversion}); terr != nil {
			return fmt.Errorf("memory_add failed on one 429 with Retry-After: %w", terr)
		}
		calls, err := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings, Contains: m})
		if err != nil {
			return err
		}
		h.Set("embedding_attempts", len(calls))
		if len(calls) < 2 || calls[0].Status != 429 || calls[len(calls)-1].Status != 200 {
			return fmt.Errorf("want a 429 then a 200, the mock recorded %v", statuses(calls))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("memory_add_works_after_the_faults", func(*core.StepHandle) error {
		m := marker(t) + "healed"
		if _, terr := s.tool("memory_add", map[string]any{"text": "stored once the provider is back " + m, "conversation_id": s.conversion}); terr != nil {
			return terr
		}
		out, terr := s.tool("memory_search", map[string]any{"query": m, "conversation_id": s.conversion})
		if terr != nil {
			return terr
		}
		if !strings.Contains(out, m) {
			return fmt.Errorf("memory_search did not find the memory added after the faults: %s", out)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("extraction_is_retried_on_a_500", func(h *core.StepHandle) error {
		m := marker(t) + "extract"
		f := llmmock.HTTPStatus(500)
		f.Route, f.Match = llmmock.RouteChat, m
		fid, err := s.llm.AddFault(ctx, f)
		if err != nil {
			return err
		}
		defer s.llm.RemoveFault(context.Background(), fid)
		if _, terr := s.tool("conversation_add_message", map[string]any{
			"conversation_id": s.conversion, "input": "Remember that I like " + m + ".", "output": "ok",
		}); terr != nil {
			return terr
		}
		calls, err := waitCall(t, s.llm, llmmock.RouteChat, m, 2, extractionWithin)
		if err != nil {
			return fmt.Errorf("the client gave up after the first failed extraction call: %w", err)
		}
		h.Set("extraction_attempts", len(calls))
		return nil
	})
}

func statuses(calls []llmmock.Call) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = c.Status
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
