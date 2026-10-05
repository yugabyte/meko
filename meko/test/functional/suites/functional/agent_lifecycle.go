package functional

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "functional/agent_lifecycle",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamAPIServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Create an agent, see a second create with the same id refused with 409 and one with no id with 400, find it in the list, delete it, and see it gone from the list and a second delete answer 404.",
		Run:         agentLifecycle,
	})
}

func agentLifecycle(t *core.T) error {
	api := apiFor(t)
	id := "mekoqa-" + t.ResourceSuffix()
	details := json.RawMessage(`{"description":"mekoqa agent_lifecycle"}`)
	created, deleted := false, false
	defer func() {
		if !created || deleted {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = api.DeleteAgent(ctx, id)
	}()

	if err := t.Step("create", func(h *core.StepHandle) error {
		a, err := api.CreateAgent(t, id, details)
		if err != nil {
			return err
		}
		created = true
		h.Set("agent_id", a.AgentID)
		if a.AgentID != id {
			return fmt.Errorf("create answered agent_id %q, want %q", a.AgentID, id)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("duplicate_refused", func(*core.StepHandle) error {
		_, err := api.CreateAgent(t, id, details)
		if st := mekoapi.StatusOf(err); st != http.StatusConflict {
			return fmt.Errorf("second create with id %s: %v, want a 409", id, err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("empty_id_refused", func(*core.StepHandle) error {
		_, err := api.CreateAgent(t, "", details)
		if st := mekoapi.StatusOf(err); st != http.StatusBadRequest {
			return fmt.Errorf("create with no agent_id: %v, want a 400", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("list_contains_it", func(*core.StepHandle) error {
		found, err := agentListed(t, api, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("agent %s is not in the list", id)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("delete", func(*core.StepHandle) error {
		if err := api.DeleteAgent(t, id); err != nil {
			return err
		}
		deleted = true
		return nil
	}); err != nil {
		return err
	}

	return t.Step("gone", func(*core.StepHandle) error {
		found, err := agentListed(t, api, id)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("agent %s is still listed after delete", id)
		}
		if err := api.DeleteAgent(t, id); !mekoapi.IsNotFound(err) {
			return fmt.Errorf("second delete: %v, want a 404", err)
		}
		return nil
	})
}

func agentListed(t *core.T, api *mekoapi.Client, id string) (bool, error) {
	list, err := api.ListAgents(t)
	if err != nil {
		return false, err
	}
	for _, a := range list {
		if a.AgentID == id {
			return true, nil
		}
	}
	return false, nil
}
