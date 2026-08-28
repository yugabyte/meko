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
		Name:        "features/amp_extension_hexabridge",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "hexabridge (Oracle compatibility) on an opted-in endpoint: the library is genuinely in shared_preload_libraries, Oracle to_char formats a number, and dbms_output.put_line executes — both through the extension's C library, not just its SQL scripts.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionHexabridge,
	})
}

func runAmpExtensionHexabridge(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_hexabridge", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionHexabridgeSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

// The two routines exercised are the ones the extension's own regression suite
// ships expected outputs for (test/integration/compute/ext_regress_expected/
// hexabridge): `hexabridge.to_char` and `dbms_output.put_line`.
func extensionHexabridgeSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "hexabridge", "hexabridge")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	// hexabridge's _PG_init registers a dbms_scheduler worker and dbms_alert /
	// dbms_pipe shared memory; installed-but-not-preloaded is the failure
	// CREATE EXTENSION cannot catch.
	if err := assertPreloaded(ctx, c, "hexabridge"); err != nil {
		return err
	}

	// Oracle TO_CHAR: '999.99' renders with a leading sign slot, so compare
	// trimmed — asserting the digits, not Oracle's sign-space convention.
	got, err := c.QueryString(ctx, "SELECT hexabridge.to_char(123.45, '999.99')")
	if err != nil {
		return fmt.Errorf("hexabridge.to_char(123.45, '999.99'): %w", err)
	}
	if strings.TrimSpace(got) != "123.45" {
		return fmt.Errorf("hexabridge.to_char(123.45, '999.99') = %q, want %q ignoring the sign slot — Oracle number formatting is not computing", got, "123.45")
	}

	// put_line goes through the C library (an unloaded .so fails here the way
	// LOAD 'age' did on a missing library), which is the half CREATE EXTENSION's
	// SQL scripts cannot prove. The buffer read-back is left to the extension's
	// own regression suite: its get_line signature is not published, and a
	// guessed signature fails the test for a non-product reason.
	if _, err := c.Exec(ctx, "SELECT dbms_output.put_line('amp hexabridge canary')"); err != nil {
		return fmt.Errorf("dbms_output.put_line: %w", err)
	}
	return nil
}
