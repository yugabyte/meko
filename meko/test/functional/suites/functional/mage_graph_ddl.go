package functional

import (
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name: "functional/mage_graph_ddl", Suite: "functional", Category: core.Functional, Tags: tags("universe"), OwningTeam: core.TeamPlatform, Unguarded: true,
		Env: clusterOnly(), Timeout: 30 * time.Minute,
		Description: "Data migration 000001 creates its graphs the way golang-migrate sends it, as one query: create_graph then create_vlabel. That keeps the backend alive with read committed both on, as yugabyted runs, and off, as a YBA universe runs by default.",
		Params:      []string{"db"},
		Run:         mageGraphDDL,
	})
}

// psql's \; joins the statements into one simple query, as golang-migrate sends a
// migration file.
const graphDDLInOneQuery = `SET search_path = mag_catalog, public \; SELECT * FROM create_graph('%[1]s') \; SELECT * FROM create_vlabel('%[1]s', 'Entity');`

func mageGraphDDL(t *core.T) error {
	set := stacktest.Universes(t)
	var errs []error
	for _, rc := range []bool{true, false} {
		name := fmt.Sprintf("rc-%t", rc)
		flags := map[string]string{"ysql_yb_enable_mage": "true", "allowed_preview_flags_csv": "ysql_yb_enable_mage"}
		spec := universe.Single("mage-"+name, 1, 1)
		spec.MasterGflags = flags
		spec.TServerGflags = map[string]string{"yb_enable_read_committed_isolation": fmt.Sprint(rc)}
		for k, v := range flags {
			spec.TServerGflags[k] = v
		}
		err := step(t, "read_committed_"+fmt.Sprint(rc), func(h *core.StepHandle) error {
			u, err := set.Create(t, spec)
			if err != nil {
				return err
			}
			if _, err := u.SQL(t, "", "", "CREATE EXTENSION IF NOT EXISTS mage"); err != nil {
				return err
			}
			out, err := u.SQL(t, "", "", fmt.Sprintf(graphDDLInOneQuery, "meko_graph_ddl"))
			if err != nil {
				return fmt.Errorf("create_graph then create_vlabel in one query, yb_enable_read_committed_isolation=%t: %w", rc, err)
			}
			h.Set("output", strings.TrimSpace(out))
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}
	return nil
}
