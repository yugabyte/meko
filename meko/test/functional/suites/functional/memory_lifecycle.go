package functional

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/memory_lifecycle",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Description: "A memory's whole life across MCP and REST, as an owner, a viewer of the shared datapack and a stranger. memory_add stores and embeds, memory_search ranks the " +
			"matching memory first, get_by_id and get_all return it, the REST memories page, its text filter, the count and datapack_describe agree; memory_update re-embeds and " +
			"search follows the new text; a viewer's REST memory list holds every memory but names no owner, MCP search still finds the owner's memory, and the viewer's delete is 403; a stranger learns nothing over REST or MCP; deletion by id over MCP and REST and " +
			"memory_delete_all empty every view.",
		Run: memoryLifecycle,
	})
}

type memResult struct {
	ID       string  `json:"id"`
	Memory   string  `json:"memory"`
	Event    string  `json:"event"`
	Score    float64 `json:"score"`
	Raw      float64 `json:"raw_similarity_score"`
	Metadata struct {
		Datapack string `json:"meko_datapack_id"`
	} `json:"metadata"`
}

type memPage struct {
	Memories []struct {
		ID        string `json:"id"`
		Data      string `json:"data"`
		UserEmail string `json:"user_email"`
	} `json:"memories"`
	Total int `json:"total"`
}

func memoryLifecycle(t *core.T) error {
	accts, done := accountsFor(t, 3)
	defer done()
	var owner, viewer, stranger *user
	var err error
	if owner, err = newUser(t, accts[0]); err != nil {
		return err
	}
	defer owner.close()
	if viewer, err = newUser(t, accts[1]); err != nil {
		return err
	}
	defer viewer.close()
	if stranger, err = newUser(t, accts[2]); err != nil {
		return err
	}
	defer stranger.close()
	llm := stackEnv(t).LLM()
	mk := marker(t)

	dp, err := ownedDatapack(t, owner, "mem-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, "viewer"); err != nil {
		return err
	}
	conv, err := owner.conversation(dp.ID, "memory lifecycle "+mk)
	if err != nil {
		return err
	}
	scope := func(extra map[string]any) map[string]any {
		out := map[string]any{"datapack_id": dp.ID, "conversation_id": conv}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	facts := []string{
		fmt.Sprintf("The user %s keeps a teal kite in the attic.", mk),
		fmt.Sprintf("The user %s drinks jasmine tea every morning at the harbour.", mk),
		fmt.Sprintf("The user %s rides a violet bicycle to the observatory.", mk),
	}
	ids := make([]string, len(facts))

	if err := t.Step("add_stores_each_memory", func(h *core.StepHandle) error {
		for i, f := range facts {
			var out struct {
				Results []memResult `json:"results"`
			}
			if err := owner.callJSON("memory_add", scope(map[string]any{"text": f}), &out); err != nil {
				return err
			}
			if len(out.Results) != 1 || out.Results[0].ID == "" || out.Results[0].Memory != f || out.Results[0].Event != "ADD" {
				return fmt.Errorf("memory_add of %q answered %+v, want one ADD result with the text", f, out.Results)
			}
			ids[i] = out.Results[0].ID
		}
		h.Set("ids", strings.Join(ids, ","))
		return nil
	}); err != nil {
		return err
	}

	search := func(u *user, q string) ([]memResult, error) {
		var out struct {
			Results []memResult `json:"results"`
		}
		err := u.callJSON("memory_search", scope(map[string]any{"query": q}), &out)
		return out.Results, err
	}

	if err := t.Step("search_ranks_the_matching_memory_first", func(*core.StepHandle) error {
		for i, f := range facts {
			res, err := search(owner, f)
			if err != nil {
				return err
			}
			if len(res) == 0 || res[0].ID != ids[i] {
				var got []string
				for _, r := range res {
					got = append(got, fmt.Sprintf("%s score %.3f raw %.3f %q", r.ID, r.Score, r.Raw, r.Memory))
				}
				return fmt.Errorf("memory_search for the exact text of memory %s ranked it below another: %s", ids[i], strings.Join(got, "; "))
			}
			if res[0].Metadata.Datapack != dp.ID {
				return fmt.Errorf("memory %s carries datapack %q, want %s", ids[i], res[0].Metadata.Datapack, dp.ID)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	count := func() (int, error) {
		var c struct {
			Count int `json:"count"`
		}
		err := owner.rest(get, "/memories/count?datapack_id="+dp.ID, nil, httpOK, &c)
		return c.Count, err
	}
	var errs []error
	if err := t.Step("every_view_agrees", func(h *core.StepHandle) error {
		var one memResult
		if err := owner.callJSON("memory_get_by_id", scope(map[string]any{"memory_id": ids[0]}), &one); err != nil {
			return err
		}
		if one.ID != ids[0] || one.Memory != facts[0] {
			return fmt.Errorf("memory_get_by_id %s answered %+v", ids[0], one)
		}
		var all memPage
		if err := owner.callJSON("memory_get_all", scope(nil), &all); err != nil {
			return err
		}
		got := idsOf(all.Memories, func(m struct {
			ID        string `json:"id"`
			Data      string `json:"data"`
			UserEmail string `json:"user_email"`
		}) string {
			return m.ID
		})
		for _, id := range ids {
			if !contains(got, id) {
				return fmt.Errorf("memory_get_all lacks %s: %v", id, got)
			}
		}
		var page memPage
		if err := owner.rest(get, "/memories?page_size=100&datapack_id="+dp.ID, nil, httpOK, &page); err != nil {
			return err
		}
		if page.Total != len(facts) || len(page.Memories) != len(facts) {
			return fmt.Errorf("GET /memories lists %d of total %d, want %d", len(page.Memories), page.Total, len(facts))
		}
		for _, m := range page.Memories {
			if m.UserEmail != owner.acct.Email {
				return fmt.Errorf("memory %s is attributed to %q, want the owner %s", m.ID, m.UserEmail, owner.acct.Email)
			}
		}
		var filtered memPage
		if err := owner.rest(get, "/memories?q=jasmine&datapack_id="+dp.ID, nil, httpOK, &filtered); err != nil {
			return err
		}
		if len(filtered.Memories) != 1 || filtered.Memories[0].ID != ids[1] {
			return fmt.Errorf("GET /memories?q=jasmine answered %+v, want only %s", filtered.Memories, ids[1])
		}
		n, err := count()
		if err != nil {
			return err
		}
		var desc struct {
			MemoryCount int `json:"memory_count"`
		}
		if err := owner.callJSON("datapack_describe", map[string]any{"datapack_id": dp.ID, "conversation_id": conv}, &desc); err != nil {
			return err
		}
		h.Set("count", n)
		if n != len(facts) || desc.MemoryCount != len(facts) {
			return fmt.Errorf("the count is %d and datapack_describe says %d, want %d", n, desc.MemoryCount, len(facts))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	updated := fmt.Sprintf("The user %s keeps a crimson kite in the garage.", mk)
	if err := t.Step("update_reembeds", func(*core.StepHandle) error {
		var before int
		if llm != nil {
			calls, err := llm.Calls(t, llmmock.Filter{Route: llmmock.RouteEmbeddings})
			if err != nil {
				return err
			}
			for _, c := range calls {
				before = max(before, c.Seq)
			}
		}
		if _, terr := owner.call("memory_update", scope(map[string]any{"memory_id": ids[0], "text": updated})); terr != nil {
			return terr
		}
		var one memResult
		if err := owner.callJSON("memory_get_by_id", scope(map[string]any{"memory_id": ids[0]}), &one); err != nil {
			return err
		}
		if one.Memory != updated {
			return fmt.Errorf("after memory_update, get_by_id answers %q, want %q", one.Memory, updated)
		}
		res, err := search(owner, updated)
		if err != nil {
			return err
		}
		if len(res) == 0 || res[0].ID != ids[0] || res[0].Memory != updated {
			return fmt.Errorf("memory_search for the new text answered %+v, want %s with it first", res, ids[0])
		}
		if llm != nil {
			calls, err := llm.Calls(t, llmmock.Filter{Route: llmmock.RouteEmbeddings, After: before, Contains: "crimson kite"})
			if err != nil {
				return err
			}
			if len(calls) == 0 {
				return errors.New("memory_update made no embedding call for the new text, so search would still match the old one")
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_reads_but_cannot_delete", func(*core.StepHandle) error {
		var ownerPage memPage
		if err := owner.rest(get, "/memories?datapack_id="+dp.ID, nil, httpOK, &ownerPage); err != nil {
			return err
		}
		path := "/memories?datapack_id=" + dp.ID
		r, err := viewer.api.Raw(t, get, path, nil)
		if err != nil {
			return err
		}
		if err := wantStatus("the viewer's GET "+path, r, httpOK, ""); err != nil {
			return err
		}
		var env struct {
			Data memPage `json:"data"`
		}
		if err := json.Unmarshal(r.Body, &env); err != nil {
			return fmt.Errorf("decode the viewer's memories page: %w", err)
		}
		if env.Data.Total != ownerPage.Total || env.Data.Total == 0 {
			return fmt.Errorf("the viewer's memories page totals %d, want the owner's %d: a viewer sees every memory",
				env.Data.Total, ownerPage.Total)
		}
		for _, o := range []string{owner.acct.ID, owner.acct.Email} {
			if strings.Contains(string(r.Body), o) {
				return fmt.Errorf("the viewer's memories page names the owner (%q): %s", o, truncateBody(r.Body))
			}
		}
		res, err := search(viewer, facts[1])
		if err != nil {
			return err
		}
		if len(res) == 0 || res[0].ID != ids[1] {
			return fmt.Errorf("the viewer's memory_search answered %+v, want %s", res, ids[1])
		}
		return refused(t, "the viewer deletes the owner's memory over REST", viewer.api, del, "/memories/"+ids[1]+"?datapack_id="+dp.ID, nil,
			statusForbidden, "FORBIDDEN")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("stranger_learns_nothing", func(*core.StepHandle) error {
		for _, path := range []string{"/memories?datapack_id=" + dp.ID, "/memories/count?datapack_id=" + dp.ID} {
			if err := refused(t, "a stranger GET "+path, stranger.api, get, path, nil, statusNotFound, "", facts[1], mk); err != nil {
				return err
			}
		}
		if err := refused(t, "a stranger deletes the owner's memory", stranger.api, del, "/memories/"+ids[1]+"?datapack_id="+dp.ID, nil, statusNotFound, ""); err != nil {
			return err
		}
		for _, tool := range []string{"memory_search", "memory_get_by_id", "memory_get_all"} {
			args := map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "query": facts[1], "memory_id": ids[1]}
			if err := toolRefused(stranger, "a stranger's "+tool+" on the owner's datapack", tool, args, facts[1], mk); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("delete_by_id_over_mcp", func(*core.StepHandle) error {
		if _, terr := owner.call("memory_delete_by_id", scope(map[string]any{"memory_id": ids[1]})); terr != nil {
			return terr
		}
		if text, terr := owner.call("memory_get_by_id", scope(map[string]any{"memory_id": ids[1]})); terr == nil && strings.Contains(text, facts[1]) {
			return fmt.Errorf("memory %s still answers after memory_delete_by_id: %s", ids[1], truncate(text, 200))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("delete_over_rest_then_again", func(*core.StepHandle) error {
		path := "/memories/" + url.PathEscape(ids[2]) + "?datapack_id=" + dp.ID
		if err := owner.rest(del, path, nil, httpOK, nil); err != nil {
			return err
		}
		return refused(t, "deleting the same memory again", owner.api, del, path, nil, statusNotFound, "NOT_FOUND")
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("delete_all_empties_every_view", func(h *core.StepHandle) error {
		if _, terr := owner.call("memory_delete_all", scope(nil)); terr != nil {
			return terr
		}
		var n int
		err := eventually(t, "the memory count to reach 0", visibleWithin(t), func() (bool, error) {
			var err error
			n, err = count()
			return n == 0, err
		})
		h.Set("count", n)
		if err != nil {
			return err
		}
		res, err := search(owner, updated)
		if err != nil {
			return err
		}
		if len(res) != 0 {
			return fmt.Errorf("memory_search still finds %+v after memory_delete_all", res)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
