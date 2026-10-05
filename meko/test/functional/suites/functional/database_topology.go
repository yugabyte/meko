package functional

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "functional/database_topology",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        tags(),
		OwningTeam:  core.TeamPlatform,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "The database under the stack matches the spec it was built from: yb_servers() lists as many tservers as it asked for, in the zones it asked for, the tablets are replicated as many times as its replication factor, and the api-server and mcp-server dial that database, with no bundled one beside it.",
		Params:      []string{"db"},
		Run:         databaseTopology,
	})
}

func databaseTopology(t *core.T) error {
	e := clusterEnv(t)
	var b mekostack.Backing
	if err := t.Step("recorded_backing", func(h *core.StepHandle) error {
		got, err := e.Backing(t)
		if err != nil {
			return err
		}
		b = got
		h.Set("mode", b.Mode)
		h.Set("host", b.Host)
		h.Set("replication_factor", b.Replication)
		h.Set("tservers", b.Tservers)
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	check := func(name string, fn func(*core.StepHandle) error) {
		if err := t.Step(name, fn); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	number := func(query string) (int, error) {
		out, err := e.Stack().YSQL(t, query)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return 0, fmt.Errorf("%q answered %q, want a number", query, strings.TrimSpace(out))
		}
		return n, nil
	}

	check("tservers", func(h *core.StepHandle) error {
		n, err := number("SELECT count(*) FROM yb_servers()")
		if err != nil {
			return err
		}
		h.Set("yb_servers", n)
		if b.Tservers > 0 && n != b.Tservers {
			return fmt.Errorf("yb_servers() lists %d tservers, the %s deploy built %d", n, b.Mode, b.Tservers)
		}
		if n < 1 {
			return errors.New("yb_servers() lists no tserver")
		}
		return nil
	})

	check("placement", func(h *core.StepHandle) error {
		if len(b.Placement) == 0 {
			h.Set("not_checked", "the "+b.Mode+" database records no placement")
			return nil
		}
		out, err := e.Stack().YSQL(t, "SELECT zone || '=' || count(*) FROM yb_servers() GROUP BY zone")
		if err != nil {
			return err
		}
		got := map[string]int{}
		for _, line := range strings.Fields(out) {
			z, n, _ := strings.Cut(line, "=")
			got[z], _ = strconv.Atoi(n)
		}
		h.Set("zones", fmt.Sprint(got))
		if !maps.Equal(got, b.Placement) {
			return fmt.Errorf("yb_servers() places tservers %v, the spec asked for %v", got, b.Placement)
		}
		return nil
	})

	check("replication_factor", func(h *core.StepHandle) error {
		if b.Replication == 0 {
			h.Set("not_checked", "the "+b.Mode+" backing records no replication factor")
			return nil
		}
		n, err := number("SELECT COALESCE(max(array_length(replicas, 1)), 0) FROM yb_tablet_metadata")
		if err != nil {
			return err
		}
		h.Set("replicas_per_tablet", n)
		if n != b.Replication {
			return fmt.Errorf("the tablets have %d replicas, the %s deploy was told RF%d", n, b.Mode, b.Replication)
		}
		return nil
	})

	check("stack_dials_the_backing", func(h *core.StepHandle) error {
		want := b.Host
		for _, cm := range []string{"meko-api-server-config", "meko-mcp-server-config"} {
			got, err := e.Kube().Get(t, "configmap", cm, "{.data.DB_HOST}")
			if err != nil {
				return fmt.Errorf("read %s: %w", cm, err)
			}
			h.Set(cm, got)
			if b.Mode == mekostack.ModeBundled {
				if !strings.HasPrefix(got, "yb-tservers."+e.Namespace()+".") {
					return fmt.Errorf("%s DB_HOST = %q, want the bundled database's Service in %s", cm, got, e.Namespace())
				}
			} else if got != want {
				return fmt.Errorf("%s DB_HOST = %q, want the backing at %q", cm, got, want)
			}
		}
		return nil
	})

	check("bundled_database_only_when_bundled", func(h *core.StepHandle) error {
		ws, err := e.Stack().Workloads(t)
		if err != nil {
			return err
		}
		var tservers int
		for _, w := range ws {
			if w.Kind == "statefulset" && w.Name == "yb-tserver" {
				tservers = w.Desired
			}
		}
		h.Set("bundled_tservers", tservers)
		switch {
		case b.Mode == mekostack.ModeBundled && tservers != 1:
			return fmt.Errorf("a bundled stack has %d yb-tserver replicas, want 1", tservers)
		case b.Mode != mekostack.ModeBundled && tservers != 0:
			return fmt.Errorf("the stack runs its own yb-tserver (%d replicas) beside the %s backing", tservers, b.Mode)
		}
		return nil
	})
	return errors.Join(errs...)
}
