package functional

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// A datapack created while this runs gets its project a moment later.
const langfuseProjectGrace = 45 * time.Second

var requiredExtensions = []string{"vector", "pg_dist_rag", "mage"}

func init() {
	core.Register(core.Test{
		Name:        "functional/stack_health",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamPlatform,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "Every Deployment and StatefulSet in the stack is Ready, /api/v1/health answers ok, a request that needs the schema succeeds (health 200 with a missed migration still fails everything else), and the vector, pg_dist_rag and mage extensions are installed.",
		Params:      []string{"env.mekostack"},
		Run:         stackHealth,
	})
}

func stackHealth(t *core.T) error {
	e := clusterEnv(t)
	var errs []error
	check := func(name string, fn func(*core.StepHandle) error) {
		if err := t.Step(name, fn); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	check("workloads_ready", func(h *core.StepHandle) error {
		var notReady []mekostack.Workload
		err := t.WaitFor("every Deployment and StatefulSet to be Ready", 3*time.Minute, 5*time.Second, func() (bool, error) {
			ws, err := e.Stack().Workloads(t)
			if err != nil {
				return false, err
			}
			h.Set("workloads", len(ws))
			if len(ws) == 0 {
				return false, errors.New("the namespace has no workloads")
			}
			notReady = mekostack.NotReady(ws)
			return len(notReady) == 0, nil
		})
		if err != nil {
			var parts []string
			for _, w := range notReady {
				parts = append(parts, fmt.Sprintf("%s/%s %d/%d", w.Kind, w.Name, w.Ready, w.Desired))
			}
			return fmt.Errorf("%w: not ready: %s", err, strings.Join(parts, ", "))
		}
		return nil
	})

	check("health", func(h *core.StepHandle) error {
		health, err := e.API().Health(t)
		if err != nil {
			return err
		}
		h.Set("health", fmt.Sprintf("%+v", health))
		if !health.Healthy() {
			return fmt.Errorf("health reports %+v", health)
		}
		return nil
	})

	check("schema_request", func(h *core.StepHandle) error {
		dps, err := e.API().ListDatapacks(t)
		if err != nil {
			return fmt.Errorf("GET /api/v1/datapacks: %w", err)
		}
		h.Set("datapacks", len(dps))
		return nil
	})

	check("every_datapack_has_langfuse_project", func(h *core.StepHandle) error {
		var missing []string
		err := t.WaitFor("every datapack to have a Langfuse project", langfuseProjectGrace, 3*time.Second, func() (bool, error) {
			var err error
			missing, err = e.Stack().DatapacksWithoutLangfuseProject(t)
			return err == nil && len(missing) == 0, err
		})
		h.Set("without_project", len(missing))
		if err != nil {
			return fmt.Errorf("datapacks %v have no Langfuse project, so every MCP call scoped to them fails (MEKO-621): %w", missing, err)
		}
		return nil
	})

	check("extensions", func(h *core.StepHandle) error {
		have, err := e.Stack().Extensions(t)
		if err != nil {
			return err
		}
		h.Set("extensions", strings.Join(have, ","))
		var missing []string
		for _, want := range requiredExtensions {
			if !slices.Contains(have, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing extensions %v, installed: %v", missing, have)
		}
		return nil
	})

	return errors.Join(errs...)
}
