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
		Name:       "functional/memory_promote_flow",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Description: "Learnings becoming shared knowledge. New memories wait on the learnings page; promoting one over REST and one with memory_promote moves them to the promoted list " +
			"and the knowledge-base counts, the memory's own embedding copied without a model call; keep-as-memory moves one off the learnings page onto the memories " +
			"page; memory_get_all with promoted lists what was promoted; a promoted memory deleted is gone; a malformed id, an empty list and another datapack's memory are " +
			"refused or reported cleanly, and a stranger cannot promote.",
		Run: memoryPromoteFlow,
	})
}

type promotedPage struct {
	Promoted []struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	} `json:"promoted_memories"`
	Total int `json:"total"`
}

func memoryPromoteFlow(t *core.T) error {
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
	dp, err := ownedDatapack(t, owner, "promote-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	other, err := ownedDatapack(t, owner, "promote-other-"+t.ResourceSuffix())
	defer deleteDatapack(owner, other.ID)
	if err != nil {
		return err
	}
	conv, err := owner.conversation(dp.ID, "promote "+mk)
	if err != nil {
		return err
	}
	otherConv, err := owner.conversation(other.ID, "promote other "+mk)
	if err != nil {
		return err
	}
	facts := []string{
		fmt.Sprintf("Team rule %s: every incident gets a written review within a week.", mk),
		fmt.Sprintf("Team rule %s: the on-call hands over at nine in the morning.", mk),
		fmt.Sprintf("Personal note %s: the user likes a quiet desk near the window.", mk),
	}
	ids := make([]string, len(facts))
	add := func(datapack, c, text string) (string, error) {
		var out struct {
			Results []memResult `json:"results"`
		}
		if err := owner.callJSON("memory_add", map[string]any{"datapack_id": datapack, "conversation_id": c, "text": text}, &out); err != nil {
			return "", err
		}
		if len(out.Results) == 0 {
			return "", fmt.Errorf("memory_add answered no result for %q", text)
		}
		return out.Results[0].ID, nil
	}
	if err := t.Step("memories_wait_as_learnings", func(*core.StepHandle) error {
		for i, f := range facts {
			var err error
			if ids[i], err = add(dp.ID, conv, f); err != nil {
				return err
			}
		}
		var page memPage
		if err := owner.rest(get, "/memories?scope=learnings&datapack_id="+dp.ID, nil, httpOK, &page); err != nil {
			return err
		}
		if page.Total != len(facts) {
			return fmt.Errorf("the learnings page holds %d, want the %d new memories", page.Total, len(facts))
		}
		return nil
	}); err != nil {
		return err
	}

	learnings := func(scope string) ([]string, error) {
		var page memPage
		err := owner.rest(get, "/memories?page_size=100&scope="+scope+"&datapack_id="+dp.ID, nil, httpOK, &page)
		out := make([]string, len(page.Memories))
		for i, m := range page.Memories {
			out[i] = m.ID
		}
		return out, err
	}
	promoted := func() (promotedPage, error) {
		var p promotedPage
		err := owner.rest(get, "/promoted-memories?datapack_id="+dp.ID, nil, httpOK, &p)
		return p, err
	}
	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("promote_over_rest", func() error {
		var res struct {
			Inserted []string `json:"inserted_ids"`
			NotFound []string `json:"not_found_ids"`
		}
		if err := owner.rest(post, "/promote-memories", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{ids[0]}}, httpOK, &res); err != nil {
			return err
		}
		if len(res.Inserted) != 1 || res.Inserted[0] != ids[0] {
			return fmt.Errorf("promote answered %+v, want %s inserted", res, ids[0])
		}
		p, err := promoted()
		if err != nil {
			return err
		}
		if p.Total != 1 || p.Promoted[0].ID != ids[0] || p.Promoted[0].Data != facts[0] {
			return fmt.Errorf("the promoted list is %+v, want %s with its text", p, ids[0])
		}
		l, err := learnings("learnings")
		if err != nil {
			return err
		}
		if contains(l, ids[0]) {
			return fmt.Errorf("%s is still a learning after its promotion", ids[0])
		}
		return nil
	})
	step("promote_over_mcp", func() error {
		if _, terr := owner.call("memory_promote", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "memory_ids": []string{ids[1]}}); terr != nil {
			return terr
		}
		p, err := promoted()
		if err != nil {
			return err
		}
		var got []string
		for _, m := range p.Promoted {
			got = append(got, m.ID)
		}
		if !contains(got, ids[1]) {
			return fmt.Errorf("memory_promote did not reach the promoted list: %v", got)
		}
		text, terr := owner.call("memory_get_all", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "promoted": true})
		if terr != nil {
			return terr
		}
		if !strings.Contains(text, ids[1]) {
			return fmt.Errorf("memory_get_all promoted=true lacks %s: %s", ids[1], truncate(text, 300))
		}
		var c struct {
			Promoted int `json:"promoted_memories"`
		}
		if err := owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/counts", nil, httpOK, &c); err != nil {
			return err
		}
		if c.Promoted < 2 {
			return fmt.Errorf("the knowledge-base counts show %d promoted memories, want 2", c.Promoted)
		}
		return nil
	})
	step("keep_as_memory_moves_it_to_memories", func() error {
		var res struct {
			Updated int `json:"updated"`
		}
		if err := owner.rest(post, "/keep-as-memory", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{ids[2]}}, httpOK, &res); err != nil {
			return err
		}
		l, err := learnings("learnings")
		if err != nil {
			return err
		}
		m, err := learnings("memories")
		if err != nil {
			return err
		}
		if contains(l, ids[2]) || !contains(m, ids[2]) {
			return fmt.Errorf("after keep-as-memory, learnings %v and memories %v, want %s only in memories", l, m, ids[2])
		}
		return nil
	})
	step("delete_promoted", func() error {
		if err := owner.rest(del, "/promoted-memories/"+ids[0]+"?datapack_id="+dp.ID, nil, httpOK, nil); err != nil {
			return err
		}
		p, err := promoted()
		if err != nil {
			return err
		}
		for _, m := range p.Promoted {
			if m.ID == ids[0] {
				return fmt.Errorf("promoted memory %s is still listed after its delete", ids[0])
			}
		}
		return nil
	})
	step("bad_input_is_refused_cleanly", func() error {
		if err := refused(t, "promote an empty list", owner.api, post, "/promote-memories", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{}}, 400, ""); err != nil {
			return err
		}
		if err := refused(t, "promote a malformed id", owner.api, post, "/promote-memories", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{"not-a-uuid"}}, 400, ""); err != nil {
			return err
		}
		foreign, err := add(other.ID, otherConv, fmt.Sprintf("Elsewhere %s: the other datapack's rule.", mk))
		if err != nil {
			return err
		}
		var res struct {
			Inserted []string `json:"inserted_ids"`
			NotFound []string `json:"not_found_ids"`
		}
		if err := owner.rest(post, "/promote-memories", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{foreign}}, httpOK, &res); err != nil {
			return err
		}
		if len(res.Inserted) != 0 || !contains(res.NotFound, foreign) {
			return fmt.Errorf("promoting another datapack's memory %s answered %+v, want it not found", foreign, res)
		}
		return nil
	})
	step("stranger_cannot_promote", func() error {
		return refused(t, "a stranger promotes", stranger.api, post, "/promote-memories", map[string]any{"datapack_id": dp.ID, "memory_ids": []string{ids[1]}}, statusNotFound, "", facts[1])
	})
	return errors.Join(errs...)
}
