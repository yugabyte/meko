package functional

import (
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/viewer_read_only_mcp",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "A viewer of a shared datapack over MCP with its own PAT. conversation_create, and conversation_add_message, " +
			"conversation_update and conversation_delete on the owner's conversation, and artifact_put, answer read_only_access, as do memory_add and " +
			"memory_update. The viewer's memory deletes reach only its own rows: memory_delete_by_id on the owner's memory is not_found " +
			"and memory_delete_all removes nothing of the owner's. context_search, knowledgebase_search and memory_search still work. " +
			"The owner's conversation and memory are untouched afterwards.",
		Run: viewerReadOnlyMCP,
	})
}

func viewerReadOnlyMCP(t *core.T) error {
	accts, done := accountsFor(t, 2)
	defer done()
	var us [2]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, viewer := us[0], us[1]
	mk := "vr" + t.ResourceSuffix()

	dp, err := ownedDatapack(t, owner, "dp-viewer-mcp-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, "viewer"); err != nil {
		return err
	}

	ownerConv, err := owner.conversation(dp.ID, "owner "+mk)
	if err != nil {
		return err
	}
	var turn struct {
		MessageID string `json:"message_id"`
	}
	if err := owner.callJSON("conversation_add_message", map[string]any{
		"datapack_id": dp.ID, "conversation_id": ownerConv,
		"input":  "owner " + mk + ": our deploys freeze on Fridays after noon, please remember that.",
		"output": "Understood, deploys freeze on Fridays after noon.",
	}, &turn); err != nil {
		return err
	}

	// refusedTool calls tool as the viewer and wants read_only_access.
	refusedTool := func(tool string, args map[string]any) error {
		text, terr := viewer.call(tool, args)
		if terr == nil || terr.code != "read_only_access" {
			return fmt.Errorf("the viewer's %s answered %s, want read_only_access", tool, truncate(text, 300))
		}
		return nil
	}
	var errs []error
	if err := t.Step("viewer_conversation_writes_are_refused", func(*core.StepHandle) error {
		onOwners := map[string]any{"datapack_id": dp.ID, "conversation_id": ownerConv}
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"conversation_create", map[string]any{"datapack_id": dp.ID, "title": "viewer " + mk}},
			{"conversation_add_message", map[string]any{
				"datapack_id": dp.ID, "conversation_id": ownerConv, "input": "viewer " + mk, "output": "an answer",
			}},
			{"conversation_update", map[string]any{"datapack_id": dp.ID, "conversation_id": ownerConv, "title": "renamed"}},
			{"conversation_delete", onOwners},
			{"artifact_put", map[string]any{
				"datapack_id": dp.ID, "conversation_id": ownerConv,
				"filename": "viewer.txt", "content_base64": "aGVsbG8=", "content_type": "text/plain",
			}},
		} {
			if err := refusedTool(c.tool, c.args); err != nil {
				return err
			}
		}
		if text, terr := owner.call("conversation_get", onOwners); terr != nil {
			return fmt.Errorf("the owner's conversation after the viewer's refused writes: %w (%s)", terr, truncate(text, 300))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	var memoryID string
	if err := t.Step("owner_adds_a_memory", func(h *core.StepHandle) error {
		var r struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if err := owner.callJSON("memory_add", map[string]any{
			"datapack_id": dp.ID, "conversation_id": ownerConv, "text": "The " + mk + " staging cluster is named hollyhock.",
		}, &r); err != nil {
			return err
		}
		if len(r.Results) == 0 || r.Results[0].ID == "" {
			return fmt.Errorf("memory_add answered no memory id: %+v", r)
		}
		memoryID = r.Results[0].ID
		h.Set("memory_id", memoryID)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("viewer_memory_writes_are_refused", func(*core.StepHandle) error {
		scoped := func(extra map[string]any) map[string]any {
			args := map[string]any{"datapack_id": dp.ID, "conversation_id": ownerConv}
			for k, v := range extra {
				args[k] = v
			}
			return args
		}
		for tool, args := range map[string]map[string]any{
			"memory_add":    scoped(map[string]any{"text": "the viewer wants to remember " + mk}),
			"memory_update": scoped(map[string]any{"memory_id": memoryID, "text": "overwritten by the viewer"}),
		} {
			if err := refusedTool(tool, args); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	// A viewer may delete its own memories (a demoted member clearing what it
	// wrote before), but the deletes never reach anyone else's.
	if err := t.Step("viewer_deletes_reach_only_its_own_memories", func(*core.StepHandle) error {
		args := map[string]any{"datapack_id": dp.ID, "conversation_id": ownerConv}
		_, terr := viewer.call("memory_delete_by_id", map[string]any{
			"datapack_id": dp.ID, "conversation_id": ownerConv, "memory_id": memoryID,
		})
		if terr == nil || terr.code != "not_found" {
			return fmt.Errorf("the viewer's memory_delete_by_id on the owner's memory answered %v, want not_found", terr)
		}
		if text, terr := viewer.call("memory_delete_all", args); terr != nil {
			return fmt.Errorf("the viewer's memory_delete_all failed: %w (%s)", terr, truncate(text, 300))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_can_search", func(*core.StepHandle) error {
		for _, tool := range []string{"context_search", "knowledgebase_search", "memory_search"} {
			if text, terr := viewer.call(tool, map[string]any{
				"datapack_id": dp.ID, "conversation_id": ownerConv, "query": "when do deploys freeze?",
			}); terr != nil {
				return fmt.Errorf("the viewer's %s failed: %w (%s)", tool, terr, truncate(text, 300))
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("owners_memory_is_untouched", func(*core.StepHandle) error {
		text, terr := owner.call("memory_get_by_id", map[string]any{"datapack_id": dp.ID, "conversation_id": ownerConv, "memory_id": memoryID})
		if terr != nil {
			return fmt.Errorf("the owner's memory %s after the viewer's refused writes: %w", memoryID, terr)
		}
		if want := "hollyhock"; !strings.Contains(text, want) {
			return fmt.Errorf("the owner's memory %s reads %s, want it to still say %q", memoryID, truncate(text, 300), want)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
