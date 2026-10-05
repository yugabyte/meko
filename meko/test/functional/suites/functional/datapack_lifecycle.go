package functional

import (
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "functional/datapack_lifecycle",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamAPIServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Create a datapack, find it in the list, read it back by id, delete it, and see it gone from both the list and a direct read.",
		Run:         datapackLifecycle,
	})
}

func datapackLifecycle(t *core.T) error {
	api := apiFor(t)
	stack := stackEnv(t).Stack()
	name := "dp-" + t.ResourceSuffix()
	var dp mekoapi.Datapack
	deleted := false
	defer func() {
		if dp.ID == "" || deleted {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = api.DeleteDatapack(ctx, dp.ID)
	}()

	if err := t.Step("create", func(h *core.StepHandle) error {
		var err error
		dp, err = api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name, Description: "mekoqa " + t.ResourceSuffix()})
		if err != nil {
			return err
		}
		h.Set("datapack_id", dp.ID)
		if dp.ID == "" || dp.Name != name {
			return fmt.Errorf("create answered id %q name %q, want a datapack named %q", dp.ID, dp.Name, name)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("langfuse_project_provisioned", func(h *core.StepHandle) error {
		if stack == nil {
			h.Set("skipped", "remote deployment, no database access")
			return nil
		}
		ok, err := hasLangfuseProject(t, stack, dp.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("create answered success for %s but it has no Langfuse project (MEKO-621)", dp.ID)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("list_contains_it", func(*core.StepHandle) error {
		list, err := api.ListDatapacks(t)
		if err != nil {
			return err
		}
		for _, d := range list {
			if d.ID == dp.ID {
				if d.Name != name {
					return fmt.Errorf("the list names it %q, want %q", d.Name, name)
				}
				return nil
			}
		}
		return fmt.Errorf("datapack %s is not among the %d the list returns", dp.ID, len(list))
	}); err != nil {
		return err
	}

	if err := t.Step("get", func(*core.StepHandle) error {
		got, err := api.GetDatapack(t, dp.ID)
		if err != nil {
			return err
		}
		if got.ID != dp.ID || got.Name != name {
			return fmt.Errorf("get answered %+v, want id %s name %s", got, dp.ID, name)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("delete", func(*core.StepHandle) error {
		if err := api.DeleteDatapack(t, dp.ID); err != nil {
			return err
		}
		deleted = true
		return nil
	}); err != nil {
		return err
	}

	return t.Step("gone", func(*core.StepHandle) error {
		if _, err := api.GetDatapack(t, dp.ID); !mekoapi.IsNotFound(err) {
			return fmt.Errorf("get after delete: %v, want a 404", err)
		}
		list, err := api.ListDatapacks(t)
		if err != nil {
			return err
		}
		for _, d := range list {
			if d.ID == dp.ID {
				return fmt.Errorf("datapack %s is still listed after delete", dp.ID)
			}
		}
		return nil
	})
}
