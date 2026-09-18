package core_test

import (
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/multiregion"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// A DedicatedStack test whose backing overlay raises timeouts.provision_dedicated
// gets that budget; one without an overlay keeps the run-level default. Standing
// a YBA plus a multi-region GCE universe up does not fit the 45m a co-located
// backing needs, and the budget has to follow the shape rather than a per-run flag.
func TestProvisionBudgetFollowsTheBackingOverlay(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	plain := core.EnvSpec{Shape: core.DedicatedStack}
	base := core.ResolveProvisionTimeoutFor(cfg, plain)
	if base != 45*time.Minute {
		t.Fatalf("dedicated default = %s, want 45m", base)
	}
	withShape := core.EnvSpec{Shape: core.DedicatedStack, BackingConfig: multiregion.BackingConfig}
	if got := core.ResolveProvisionTimeoutFor(cfg, withShape); got != multiregion.DefaultProvisionTimeout {
		t.Errorf("multi-region provision budget = %s, want %s", got, multiregion.DefaultProvisionTimeout)
	}
	// The overlay must not leak into the run config other tests resolve from.
	if got := core.ResolveProvisionTimeoutFor(cfg, plain); got != base {
		t.Errorf("the overlay leaked: a plain dedicated test now resolves %s, want %s", got, base)
	}
	// A shared-shape test never consults the overlay.
	shared := core.EnvSpec{Shape: core.SharedControlPlane, BackingConfig: multiregion.BackingConfig}
	if got := core.ResolveProvisionTimeoutFor(cfg, shared); got == multiregion.DefaultProvisionTimeout {
		t.Errorf("a shared-shape test must not take the dedicated overlay's budget (%s)", got)
	}
}
