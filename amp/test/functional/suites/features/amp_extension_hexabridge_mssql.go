package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_hexabridge_mssql",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "hexabridge_mssql (SQL Server compatibility) on an opted-in endpoint: it is mounted without being preloaded, installs into schema sys, and T-SQL LEN and STUFF answer with SQL Server semantics through the extension's C library.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionHexabridgeMSSQL,
	})
}

func runAmpExtensionHexabridgeMSSQL(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_hexabridge_mssql", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionHexabridgeMSSQLSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionHexabridgeMSSQLSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "hexabridge_mssql", "hexabridge_mssql")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	// Unlike its Oracle sibling it is a mount, not a restart: the catalog's
	// Preload: false must reach the chart, or every opt-in pays a restart it
	// does not need.
	libs, err := c.QueryString(ctx, "SHOW shared_preload_libraries")
	if err != nil {
		return fmt.Errorf("SHOW shared_preload_libraries: %w", err)
	}
	if containsToken(libs, "hexabridge_mssql") {
		return fmt.Errorf("shared_preload_libraries is %q: hexabridge_mssql is not a preload extension", libs)
	}

	schema, err := c.QueryString(ctx, "SELECT extnamespace::regnamespace::text FROM pg_extension WHERE extname = 'hexabridge_mssql'")
	if err != nil {
		return fmt.Errorf("read hexabridge_mssql's schema: %w", err)
	}
	if schema != "sys" {
		return fmt.Errorf("hexabridge_mssql installed into %q, want sys (its .control pins it)", schema)
	}

	// T-SQL LEN ignores trailing blanks, where Postgres's own length() says 5.
	n, err := c.QueryInt(ctx, "SELECT sys.len('abc  ')")
	if err != nil {
		return fmt.Errorf("sys.len: %w", err)
	}
	if n != 3 {
		return fmt.Errorf("sys.len('abc  ') = %d, want 3 — T-SQL LEN ignores trailing blanks", n)
	}
	got, err := c.QueryString(ctx, "SELECT sys.stuff('abcdef', 2, 3, 'XY')")
	if err != nil {
		return fmt.Errorf("sys.stuff: %w", err)
	}
	if got != "aXYef" {
		return fmt.Errorf("sys.stuff('abcdef', 2, 3, 'XY') = %q, want %q", got, "aXYef")
	}
	return nil
}
