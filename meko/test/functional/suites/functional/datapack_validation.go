package functional

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "functional/datapack_validation",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamAPIServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Datapack names shorter than 3 characters, longer than 60, or carrying an HTML tag are refused with 400 VALIDATION_ERROR and create nothing, and reading a datapack id that does not exist answers 404.",
		Run:         datapackValidation,
	})
}

func datapackValidation(t *core.T) error {
	api := apiFor(t)
	suffix := t.ResourceSuffix()
	cases := []struct{ step, name string }{
		{"too_short", "ab"},
		{"too_long", "dp-" + suffix + "-" + strings.Repeat("x", 61)},
		{"html_tag", "dp-<b>" + suffix + "</b>"},
	}
	for _, c := range cases {
		if err := t.Step(c.step, func(*core.StepHandle) error {
			dp, err := api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: c.name})
			if dp.ID != "" {
				ctx, cancel := cleanupCtx()
				defer cancel()
				_ = api.DeleteDatapack(ctx, dp.ID)
				return fmt.Errorf("name %q was accepted as datapack %s", c.name, dp.ID)
			}
			var e *mekoapi.Error
			if !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != "VALIDATION_ERROR" {
				return fmt.Errorf("name %q: %v, want a 400 VALIDATION_ERROR", c.name, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return t.Step("unknown_id_not_found", func(*core.StepHandle) error {
		const id = "00000000-0000-4000-8000-000000000000"
		if _, err := api.GetDatapack(t, id); !mekoapi.IsNotFound(err) {
			return fmt.Errorf("get %s: %v, want a 404", id, err)
		}
		return nil
	})
}
