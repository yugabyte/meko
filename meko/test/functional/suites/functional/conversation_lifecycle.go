package functional

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/conversation_lifecycle",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Description: "A conversation from creation to deletion, across the MCP tools and the web app's observe pages. Turns added with conversation_add_message come back " +
			"in order from conversation_get, the conversation is listed over MCP and REST with a count, the REST detail shows the turns and each message's trace, " +
			"a rename shows everywhere, conversation search finds a turn's words through context_search, the opt-out flag round-trips through datapack_describe, " +
			"a malformed conversation id is refused with a clean error, a stranger is refused without learning the turns, and a deleted conversation is gone from every view.",
		Run: conversationLifecycle,
	})
}

type convMessage struct {
	ID     string `json:"id"`
	Input  any    `json:"input"`
	Output any    `json:"output"`
}

type convBody struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Messages []convMessage `json:"messages"`
}

func conversationLifecycle(t *core.T) error {
	accts, done := accountsFor(t, 2)
	defer done()
	owner, err := newUser(t, accts[0])
	defer owner.close()
	if err != nil {
		return err
	}
	stranger, err := newUser(t, accts[1])
	defer stranger.close()
	if err != nil {
		return err
	}
	mk := marker(t)
	dp, err := ownedDatapack(t, owner, "conv-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	title := "observe " + mk
	var conv string
	turns := []string{
		fmt.Sprintf("Turn one %s: the deploy window is Tuesday at noon.", mk),
		fmt.Sprintf("Turn two %s: the staging cluster is named hollyhock.", mk),
		fmt.Sprintf("Turn three %s: rollbacks need two approvals.", mk),
	}
	var messageIDs []string

	if err := t.Step("create_and_add_turns", func(h *core.StepHandle) error {
		var c struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := owner.callJSON("conversation_create", map[string]any{"datapack_id": dp.ID, "title": title, "metadata": `{"source": "mekoqa"}`}, &c); err != nil {
			return err
		}
		if c.ID == "" || c.Title != title {
			return fmt.Errorf("conversation_create answered %+v, want an id and title %q", c, title)
		}
		conv = c.ID
		h.Set("conversation_id", conv)
		for i, in := range turns {
			var r struct {
				Status         string `json:"status"`
				MessageID      string `json:"message_id"`
				ConversationID string `json:"conversation_id"`
			}
			if err := owner.callJSON("conversation_add_message", map[string]any{
				"datapack_id": dp.ID, "conversation_id": conv, "input": in, "output": fmt.Sprintf("Noted %d.", i),
			}, &r); err != nil {
				return fmt.Errorf("turn %d: %w", i, err)
			}
			if r.Status != "accepted" || r.MessageID == "" || r.ConversationID != conv {
				return fmt.Errorf("turn %d answered %+v, want accepted with a message id on %s", i, r, conv)
			}
			messageIDs = append(messageIDs, r.MessageID)
		}
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	if err := t.Step("get_returns_the_turns_in_order", func(h *core.StepHandle) error {
		var got convBody
		var last string
		err := eventually(t, "conversation_get to return the 3 turns", visibleWithin(t), func() (bool, error) {
			text, terr := owner.call("conversation_get", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "include_messages": true})
			if terr != nil {
				last = terr.Error()
				return false, nil
			}
			last = text
			got = convBody{}
			if err := jsonUnmarshal(text, &got); err != nil {
				return false, err
			}
			return len(got.Messages) >= len(turns), nil
		})
		if err != nil {
			return fmt.Errorf("%w; last answer: %s", err, truncate(last, 300))
		}
		for i, m := range got.Messages[:len(turns)] {
			if !strings.Contains(fmt.Sprint(m.Input), turns[i]) {
				return fmt.Errorf("message %d is %v, want turn %q in order", i, m.Input, turns[i])
			}
		}
		if got.Title != title {
			return fmt.Errorf("conversation_get title %q, want %q", got.Title, title)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	type listed struct {
		Conversations []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"conversations"`
	}
	inList := func(l listed) (bool, string) {
		for _, c := range l.Conversations {
			if strings.ReplaceAll(c.ID, "-", "") == conv {
				return true, c.Title
			}
		}
		return false, ""
	}
	if err := t.Step("listed_over_mcp_and_rest", func(h *core.StepHandle) error {
		return eventually(t, "the conversation in conversation_list and GET /conversations", visibleWithin(t), func() (bool, error) {
			var m, r listed
			if err := owner.callJSON("conversation_list", map[string]any{"datapack_id": dp.ID, "conversation_id": conv}, &m); err != nil {
				return false, err
			}
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations", nil, httpOK, &r); err != nil {
				return false, err
			}
			var c struct {
				Count int `json:"count"`
			}
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations/count", nil, httpOK, &c); err != nil {
				return false, err
			}
			okM, _ := inList(m)
			okR, _ := inList(r)
			h.Set("mcp_listed", okM)
			h.Set("rest_listed", okR)
			h.Set("count", c.Count)
			return okM && okR && c.Count >= 1, nil
		})
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("rest_detail_and_traces", func(h *core.StepHandle) error {
		var detail map[string]any
		if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations/"+conv, nil, httpOK, &detail); err != nil {
			return err
		}
		body := fmt.Sprint(detail)
		for _, in := range turns {
			if !strings.Contains(body, in) {
				return fmt.Errorf("the REST detail lacks turn %q: %s", in, truncate(body, 300))
			}
		}
		if len(messageIDs) > 0 {
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations/"+conv+"/messages/"+messageIDs[0]+"/trace", nil, httpOK, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	renamed := "renamed " + mk
	if err := t.Step("rename_shows_everywhere", func(*core.StepHandle) error {
		if _, terr := owner.call("conversation_update", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "title": renamed}); terr != nil {
			return terr
		}
		var mcpTitle, restTitle string
		err := eventually(t, "the new title in conversation_get and the REST list", visibleWithin(t), func() (bool, error) {
			var got convBody
			if err := owner.callJSON("conversation_get", map[string]any{"datapack_id": dp.ID, "conversation_id": conv}, &got); err != nil {
				return false, err
			}
			var r listed
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations", nil, httpOK, &r); err != nil {
				return false, err
			}
			mcpTitle = got.Title
			_, restTitle = inList(r)
			return mcpTitle == renamed && restTitle == renamed, nil
		})
		if err != nil {
			return fmt.Errorf("%w: conversation_get says %q and the REST list says %q, want %q", err, mcpTitle, restTitle, renamed)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("conversation_search_finds_a_turn", func(h *core.StepHandle) error {
		var last string
		err := eventually(t, "context_search over conversations to return turn two", 3*visibleWithin(t), func() (bool, error) {
			text, terr := owner.call("context_search", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "query": "staging cluster hollyhock " + mk, "force_source": "conversation"})
			if terr != nil {
				return false, terr
			}
			last = text
			return strings.Contains(text, "hollyhock"), nil
		})
		if err != nil {
			return fmt.Errorf("%w; last answer: %s", err, truncate(last, 300))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("opt_out_round_trips", func(*core.StepHandle) error {
		describe := func() (bool, error) {
			var d struct {
				OptOut bool `json:"conversation_search_opt_out"`
			}
			err := owner.callJSON("datapack_describe", map[string]any{"datapack_id": dp.ID, "conversation_id": conv}, &d)
			return d.OptOut, err
		}
		for _, want := range []bool{true, false} {
			if err := owner.rest(patch, "/datapacks/"+dp.ID+"/conversation-search-opt-out", map[string]bool{"opted_out": want}, httpOK, nil); err != nil {
				return err
			}
			got, err := describe()
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("after PATCH opted_out=%v, datapack_describe says %v", want, got)
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("malformed_ids_are_refused_cleanly", func(*core.StepHandle) error {
		for _, id := range []string{"not-a-conversation-id", ""} {
			for _, tool := range []string{"conversation_get", "conversation_add_message", "memory_search"} {
				args := map[string]any{"datapack_id": dp.ID, "conversation_id": id, "input": "x", "output": "y", "query": "x"}
				if err := toolRefused(owner, fmt.Sprintf("%s with conversation_id %q", tool, id), tool, args); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("stranger_is_refused", func(*core.StepHandle) error {
		if err := refused(t, "a stranger's REST detail", stranger.api, get, "/datapacks/"+dp.ID+"/conversations/"+conv, nil, statusNotFound, "", turns[1]); err != nil {
			return err
		}
		return toolRefused(stranger, "a stranger's conversation_get", "conversation_get",
			map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "include_messages": true}, turns[1], "hollyhock")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("delete_removes_it_everywhere", func(*core.StepHandle) error {
		if _, terr := owner.call("conversation_delete", map[string]any{"datapack_id": dp.ID, "conversation_id": conv}); terr != nil {
			return terr
		}
		var stillGet, stillListed bool
		err := eventually(t, "the conversation to leave conversation_get and the REST list", visibleWithin(t), func() (bool, error) {
			_, terr := owner.call("conversation_get", map[string]any{"datapack_id": dp.ID, "conversation_id": conv})
			stillGet = terr == nil
			var r listed
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/conversations", nil, httpOK, &r); err != nil {
				return false, err
			}
			stillListed, _ = inList(r)
			return !stillGet && !stillListed, nil
		})
		if err != nil {
			return fmt.Errorf("%w: after conversation_delete, conversation_get answers %v and the REST list has it %v", err, stillGet, stillListed)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
