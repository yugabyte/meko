package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/controller/local/config"
)

// StartFailure signals that `start` failed and the caller should exit(2)
// (matching Rust `handle_start_all`, which stop-alls then `exit(2)`).
type StartFailure struct{ Err error }

func (e *StartFailure) Error() string { return e.Err.Error() }
func (e *StartFailure) Unwrap() error { return e.Err }

// StartAll brings up the whole stack: (1) provision the external database,
// (2) start the controller (waits for its migrations), (3) start the
// provisioned pageservers + safekeepers. Each daemon's
// own startup readiness check (below) already gates on the daemon being live,
// so there is no separate node-liveness poll. On any failure it stop-alls and
// returns a *StartFailure. Mirrors Rust `handle_start_all` +
// `handle_start_all_impl`.
func StartAll(ctx context.Context, env *config.LocalEnv, timeout time.Duration) error {
	if err := startAllImpl(ctx, env, timeout); err != nil {
		fmt.Println("startup failed because one or more services could not be started")
		fmt.Printf("%v\n", err)
		StopAll(ctx, env, true)
		return &StartFailure{Err: err}
	}
	fmt.Println("\nybAmp started")
	return nil
}

func startAllImpl(ctx context.Context, env *config.LocalEnv, timeout time.Duration) error {
	// Phase 1 — provision the external yb_storage database.
	//
	// Create-if-missing on purpose: `start` must not drop the database. The
	// pageservers Phase 3 brings up are rediscovered from disk
	// (endpoints/*/pageserver/pageserver.toml) and each expects its
	// amp_storage.branch_state row to be there already, so wiping the database
	// here would leave those rows gone and every start after the first
	// `endpoint create` would fail. Resetting the database belongs with resetting the
	// on-disk state, which is `init` — see recreateExternalDatabase.
	if env.Controller.Yb != nil {
		if err := ensureExternalDatabase(ctx, env.Controller.Yb); err != nil {
			return fmt.Errorf("prepare external controller database: %w", err)
		}
	}

	// Phase 2 — controller (runs diesel migrations; must finish before the
	// storage daemons connect to yb_storage).
	if err := startController(ctx, env, 1, timeout); err != nil {
		return fmt.Errorf("start controller: %w", err)
	}

	// Phase 3 — one supervisor per endpoint, which brings up that endpoint's
	// safekeeper, pageserver and compute in dependency order.
	//
	// Computes included: an endpoint is up or down as a unit, because the compute
	// cannot serve without the two below it.
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return fmt.Errorf("load endpoints: %w", err)
	}
	for _, ep := range endpoints.all {
		// Daemon config first: the supervisor is told it brings the tomls, and
		// refuses to start when one is missing. The compute spec is its own to
		// render, so there is nothing to write here.
		if err := renderDaemonConfig(env, ep, "safekeeper"); err != nil {
			return fmt.Errorf("render safekeeper config for %s: %w", ep.conf.EndpointID, err)
		}
		if err := startEndpointSupervisor(env, ep, timeout); err != nil {
			return fmt.Errorf("start endpoint %s: %w", ep.conf.EndpointID, err)
		}
	}
	return nil
}

// StopAll stops every endpoint compute, pageserver,
// safekeeper and controller instance, best-effort. Mirrors Rust
// `try_stop_all`. Used by the `start` failure-cleanup path.
func StopAll(ctx context.Context, env *config.LocalEnv, immediate bool) {
	mode := StopModeFast
	if immediate {
		mode = StopModeImmediate
	}
	if endpoints, err := loadEndpoints(ctx, env); err == nil {
		// Over `all`: stopping everything means everything, including an endpoint
		// the by-name index dropped because another branch uses the same name.
		for _, ep := range endpoints.all {
			if err := stopEndpoint(ctx, env, ep, mode, false); err != nil {
				fmt.Printf("postgres stop failed: %v\n", err)
			}
		}
	} else {
		fmt.Printf("postgres stop failed, could not restore control plane data from env: %v\n", err)
	}

	// Then the supervisors, which take the pageservers and safekeepers with them.
	// After the computes above: a supervisor that goes first takes its compute's
	// storage away mid-shutdown.
	for _, sk := range env.Safekeepers {
		if err := stopEndpointSupervisor(env, sk.EndpointID, immediate); err != nil {
			fmt.Printf("supervisor for endpoint %s stop failed: %v\n", sk.EndpointID, err)
		}
	}
	ids, err := controllerInstances(env)
	if err != nil {
		return
	}
	for _, id := range ids {
		if err := stopController(env, id, immediate); err != nil {
			fmt.Printf("Controller instance %d stop failed: %v\n", id, err)
		}
	}
}
