package functional

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

func init() {
	core.Register(core.Test{
		Name: "functional/xcluster_basic", Suite: "functional", Category: core.Functional, Tags: tags("universe"), OwningTeam: core.TeamPlatform, Unguarded: true,
		Env: env(), Timeout: 40 * time.Minute,
		Description: "Two universes, xCluster replication of one database from the first to the second, a table created and a row written on the source, and the row read on the target. A provider that bootstraps the target from a backup keeps it in the stack's MinIO.",
		Params:      []string{"db"},
		Run:         xclusterBasic,
	})
}

func xclusterBasic(t *core.T) error {
	var x universe.XCluster
	src, dst, err := pair(t, func(src universe.Universe) { x = need[universe.XCluster](t, src) })
	if err != nil {
		return finish(t, err)
	}
	b, bootstraps := x.(universe.XClusterBootstrap)
	if bootstraps {
		b.BootstrapXClusterVia(stackS3(t))
	}

	if err := step(t, "source_database", func(*core.StepHandle) error {
		if _, err := src.SQL(t, "", "", "CREATE DATABASE app"); err != nil || !bootstraps {
			return err
		}
		// YBA finds a database to replicate through its tables. yugabyted's
		// automatic mode needs the source empty, its target is not bootstrapped.
		// kv, the table read on the target, is created after setup so DDL
		// replication is tested.
		_, err := src.SQL(t, "", "app", "CREATE TABLE seed(k int primary key)")
		return err
	}); err != nil {
		return err
	}
	if err := step(t, "setup_replication", func(*core.StepHandle) error {
		return (x.SetupXCluster(t, dst, []string{"app"}))
	}); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := cleanup()
		defer cancel()
		_ = x.TeardownXCluster(ctx, dst)
	}()
	if err := step(t, "replication_healthy", func(h *core.StepHandle) error {
		st, err := x.XClusterStatus(t, dst)
		if err != nil {
			return err
		}
		h.Set("detail", st.Detail)
		if !st.Healthy {
			return fmt.Errorf("replication is not healthy: %s", st.Detail)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := step(t, "write_on_source", func(*core.StepHandle) error {
		_, err := src.SQL(t, "", "app", "CREATE TABLE kv(k int primary key, v text); INSERT INTO kv VALUES (1, 'from-source')")
		return err
	}); err != nil {
		return err
	}
	return step(t, "read_on_target", func(*core.StepHandle) error {
		return waitRow(t, dst, "", "app", "SELECT v FROM kv WHERE k = 1", "from-source", 3*time.Minute)
	})
}
