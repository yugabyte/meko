package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_pgaudit",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "pgaudit on an opted-in endpoint: the library is preloaded, its check hook rejects a bad pgaudit.log, its two event triggers are installed and enabled, and DDL runs through them with auditing on.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionPgaudit,
	})
}

func runAmpExtensionPgaudit(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_pgaudit", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionPgauditSteps(ctx, cl, a.proj, "pgaudit_probe_"+a.f.suffix(), a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionPgauditSteps(ctx context.Context, cl ampCtl, proj ampCluster, table string, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "pgaudit", "pgaudit")
	if err != nil || !ok {
		return err
	}
	defer c.Close()
	if err := assertPreloaded(ctx, c, "pgaudit"); err != nil {
		return err
	}

	// Without the library loaded, pgaudit.log is a placeholder GUC that takes any
	// value; only the loaded library's check hook refuses this one.
	if _, err := c.Exec(ctx, "SET pgaudit.log = 'bogus'"); err == nil {
		return fmt.Errorf("SET pgaudit.log = 'bogus' succeeded: the pgaudit library is not loaded in this backend")
	} else if !strings.Contains(err.Error(), "pgaudit.log") {
		return fmt.Errorf("SET pgaudit.log = 'bogus' failed for another reason than pgaudit's check hook: %w", err)
	}

	n, err := c.QueryInt(ctx, `SELECT count(*) FROM pg_event_trigger
		WHERE evtname IN ('pgaudit_ddl_command_end', 'pgaudit_sql_drop') AND evtenabled <> 'D'`)
	if err != nil {
		return fmt.Errorf("read pgaudit's event triggers: %w", err)
	}
	if n != 2 {
		return fmt.Errorf("found %d of pgaudit's 2 enabled event triggers (pgaudit_ddl_command_end, pgaudit_sql_drop)", n)
	}

	// Both triggers fire on DDL and call into the library, so a create and a drop
	// with auditing on prove the C functions behind them load and run. One
	// statement string, because c is a pool and a session SET may not carry over.
	if _, err := c.Exec(ctx, "BEGIN; SET LOCAL pgaudit.log = 'ddl'; CREATE TABLE "+table+" (id int); DROP TABLE "+table+"; COMMIT"); err != nil {
		return fmt.Errorf("CREATE and DROP TABLE through pgaudit's event triggers with pgaudit.log = 'ddl': %w", err)
	}
	return nil
}
