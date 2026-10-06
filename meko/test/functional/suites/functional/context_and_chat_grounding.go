package functional

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/context_and_chat_grounding",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    20 * time.Minute,
		Description: "Recall across every source an agent has, and the playground answering from it. With a memory, an indexed document and a conversation turn in one datapack " +
			"and a memory in another, context_search returns each from its own source, force_source narrows it, an unknown source is a clean error, nothing crosses from the " +
			"other datapack, and the search shows in token savings and datapack metrics. The gateway's /chat on the datapack sends the model a prompt carrying the memory, " +
			"answers with the model's reply, reports the hits it used, keeps a resumed conversation, and the turn is stored in the conversation.",
		Run: contextAndChatGrounding,
	})
}

type contextResult struct {
	Results struct {
		Memory        []map[string]any `json:"memory"`
		KnowledgeBase []map[string]any `json:"knowledge_base"`
		Conversation  []map[string]any `json:"conversation"`
	} `json:"results"`
}

func contextAndChatGrounding(t *core.T) error {
	accts, done := accountsFor(t, 1)
	defer done()
	owner, err := newUser(t, accts[0])
	defer owner.close()
	if err != nil {
		return err
	}
	mk := marker(t)
	dp, err := ownedDatapack(t, owner, "ctx-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	other, err := ownedDatapack(t, owner, "ctx-other-"+t.ResourceSuffix())
	defer deleteDatapack(owner, other.ID)
	if err != nil {
		return err
	}
	conv, err := owner.conversation(dp.ID, "grounding "+mk)
	if err != nil {
		return err
	}
	otherConv, err := owner.conversation(other.ID, "elsewhere "+mk)
	if err != nil {
		return err
	}
	memory := fmt.Sprintf("The user %s prefers the amber teapot for green tea.", mk)
	doc := fmt.Sprintf("Field manual %s: the tidal gauge at the north pier is calibrated every equinox.\n", mk)
	turn := fmt.Sprintf("Turn %s: the release train leaves on Thursdays from platform quince.", mk)
	elsewhere := fmt.Sprintf("Elsewhere %s: the vault code is written on the walnut cabinet.", mk)

	if err := t.Step("seed_every_source", func(*core.StepHandle) error {
		if _, terr := owner.call("memory_add", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "text": memory}); terr != nil {
			return terr
		}
		if _, terr := owner.call("memory_add", map[string]any{"datapack_id": other.ID, "conversation_id": otherConv, "text": elsewhere}); terr != nil {
			return terr
		}
		if _, terr := owner.call("conversation_add_message", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "input": turn, "output": "Noted."}); terr != nil {
			return terr
		}
		return nil
	}); err != nil {
		return err
	}
	docErr := t.Step("seed_a_document", func(*core.StepHandle) error {
		up, err := owner.api.UploadKBFile(t, dp.ID, "manual-"+mk+".txt", []byte(doc))
		if err != nil {
			return err
		}
		return waitIndexed(t, owner.api, dp.ID, up.DocumentKey)
	})

	search := func(datapack, convID, q, source string) (contextResult, string, *toolError) {
		args := map[string]any{"datapack_id": datapack, "conversation_id": convID, "query": q}
		if source != "" {
			args["force_source"] = source
		}
		text, terr := owner.call("context_search", args)
		var r contextResult
		if terr == nil {
			if err := json.Unmarshal([]byte(text), &r); err != nil {
				return r, text, &toolError{tool: "context_search", text: "answer is not the documented JSON: " + truncate(text, 200)}
			}
		}
		return r, text, terr
	}
	has := func(items []map[string]any, word string) bool { return strings.Contains(fmt.Sprint(items), word) }

	var errs []error
	if docErr != nil {
		errs = append(errs, docErr)
	}
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("each_source_answers", func() error {
		r, text, terr := search(dp.ID, conv, "amber teapot green tea "+mk, "")
		if terr != nil {
			return terr
		}
		if !has(r.Results.Memory, "amber teapot") {
			return fmt.Errorf("context_search did not return the memory: %s", truncate(text, 300))
		}
		if docErr == nil {
			r, text, terr = search(dp.ID, conv, "tidal gauge north pier equinox "+mk, "")
			if terr != nil {
				return terr
			}
			if !has(r.Results.KnowledgeBase, "tidal gauge") {
				return fmt.Errorf("context_search did not return the document: %s", truncate(text, 300))
			}
		}
		var last string
		err := eventually(t, "context_search to return the conversation turn", 3*visibleWithin(t), func() (bool, error) {
			r, text, terr := search(dp.ID, conv, "release train platform quince "+mk, "")
			if terr != nil {
				return false, terr
			}
			last = text
			return has(r.Results.Conversation, "quince"), nil
		})
		if err != nil {
			return fmt.Errorf("%w; last answer: %s", err, truncate(last, 300))
		}
		return nil
	})
	step("force_source_narrows", func() error {
		r, text, terr := search(dp.ID, conv, "amber teapot tidal gauge "+mk, "memory")
		if terr != nil {
			return terr
		}
		if len(r.Results.KnowledgeBase) > 0 || len(r.Results.Conversation) > 0 || !has(r.Results.Memory, "amber") {
			return fmt.Errorf("force_source=memory answered %s, want only the memory", truncate(text, 300))
		}
		r, text, terr = search(dp.ID, conv, "amber teapot tidal gauge "+mk, "knowledge_base")
		if terr != nil {
			return terr
		}
		if len(r.Results.Memory) > 0 || docErr == nil && !has(r.Results.KnowledgeBase, "tidal") {
			return fmt.Errorf("force_source=knowledge_base answered %s, want only the document", truncate(text, 300))
		}
		_, text, terr = search(dp.ID, conv, "anything", "everything")
		if terr == nil || terr.code != "invalid_force_source" {
			return fmt.Errorf("force_source=everything answered %s, want the invalid_force_source error", truncate(text, 200))
		}
		return nil
	})
	step("nothing_crosses_datapacks", func() error {
		_, text, terr := search(dp.ID, conv, "vault code walnut cabinet "+mk, "")
		if terr != nil {
			return terr
		}
		if strings.Contains(text, "walnut") {
			return fmt.Errorf("a search in %s returned the other datapack's memory: %s", dp.ID, truncate(text, 300))
		}
		_, text, terr = search(other.ID, otherConv, "amber teapot tidal gauge platform quince "+mk, "")
		if terr != nil {
			return terr
		}
		for _, w := range []string{"amber", "tidal", "quince"} {
			if strings.Contains(text, w) {
				return fmt.Errorf("a search in %s returned %q from %s: %s", other.ID, w, dp.ID, truncate(text, 300))
			}
		}
		return nil
	})
	step("searches_count_in_metrics", func() error {
		return eventually(t, "the searches to show in token savings and metrics", visibleWithin(t), func() (bool, error) {
			var ts struct {
				Requests int `json:"total_requests"`
			}
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/token-savings", nil, httpOK, &ts); err != nil {
				return false, err
			}
			var m struct {
				Requests int `json:"total_requests"`
			}
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/metrics", nil, httpOK, &m); err != nil {
				return false, err
			}
			return ts.Requests > 0 && m.Requests > 0, nil
		})
	})
	step("chat_answers_from_the_memory", func() error { return chatGrounded(t, owner, dp.ID, mk, memory) })
	return errors.Join(errs...)
}

type gatewayChat struct {
	Message        string           `json:"message"`
	ConversationID string           `json:"conversation_id"`
	MemoryHits     []map[string]any `json:"memory_hits"`
	KBHits         []map[string]any `json:"kb_hits"`
	MemoryContext  string           `json:"memory_context"`
}

func chatGrounded(t *core.T, owner *user, datapack, mk, memory string) error {
	llm := stacktest.LLM(t)
	gateway, closeGateway := stacktest.Gateway(t)
	defer closeGateway()
	question := fmt.Sprintf("Which teapot does the user %s prefer for green tea?", mk)
	reply := "The amber one, scripted " + mk
	id, err := llm.AddScript(t, llmmock.Script{Match: question, System: "Meko playground assistant", Reply: reply})
	if err != nil {
		return err
	}
	defer llm.RemoveScript(context.Background(), id)

	ask := func(q, convID string) (gatewayChat, error) {
		body, _ := json.Marshal(map[string]any{"message": q, "user_token": owner.pat.Value, "user_id": owner.acct.ID, "datapack_id": datapack, "conversation_id": convID})
		ctx, cancel := context.WithTimeout(t, chatTimeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway+"/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return gatewayChat{}, fmt.Errorf("POST /chat: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		stacktest.FailOnLeak(t, "POST /chat", string(raw))
		if resp.StatusCode != http.StatusOK {
			return gatewayChat{}, fmt.Errorf("POST /chat answered %s: %s", resp.Status, truncate(string(raw), 300))
		}
		var out gatewayChat
		if err := json.Unmarshal(raw, &out); err != nil {
			return out, fmt.Errorf("decode the chat answer: %w", err)
		}
		return out, nil
	}
	first, err := ask(question, "")
	if err != nil {
		return err
	}
	if first.Message != reply || first.ConversationID == "" {
		return fmt.Errorf("chat answered %q on conversation %q, want the model's %q", first.Message, first.ConversationID, reply)
	}
	if !strings.Contains(fmt.Sprint(first.MemoryHits)+first.MemoryContext, "amber teapot") {
		return fmt.Errorf("the chat reports no hit on the memory: hits %v, context %q", first.MemoryHits, truncate(first.MemoryContext, 200))
	}
	calls, err := waitCall(t, llm, llmmock.RouteChat, question, 1, 30*time.Second)
	if err != nil {
		return err
	}
	grounded := false
	for _, c := range calls {
		if c.Answer == "script:"+id && strings.Contains(fmt.Sprint(c.Messages), "amber teapot") {
			grounded = true
		}
	}
	if !grounded {
		return fmt.Errorf("the model was asked %q without the memory %q in its prompt", question, memory)
	}
	second, err := ask(fmt.Sprintf("And what about black tea, %s?", mk), first.ConversationID)
	if err != nil {
		return err
	}
	if second.ConversationID != first.ConversationID {
		return fmt.Errorf("a resumed chat answered on conversation %s, want %s", second.ConversationID, first.ConversationID)
	}
	// The playground writes its turns as the workbench agent, and conversation_get filters by agent.
	var last string
	err = eventually(t, "the chat turn in the conversation", visibleWithin(t), func() (bool, error) {
		text, terr := owner.call("conversation_get", map[string]any{"datapack_id": datapack, "conversation_id": first.ConversationID,
			"agent_id": "WorkbenchLM", "include_messages": true})
		if terr != nil {
			last = terr.Error()
			return false, nil
		}
		last = text
		return strings.Contains(text, question), nil
	})
	if err != nil {
		return fmt.Errorf("%w; last answer: %s", err, truncate(last, 300))
	}
	return nil
}
