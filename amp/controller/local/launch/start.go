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
// provisioned pageservers + safekeepers + endpoint_storages. Each daemon's
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

	// Phase 3 — pageservers (only those with a baked branch identity),
	// safekeepers, endpoint_storages.
	for i := range env.Pageservers {
		ps := &env.Pageservers[i]
		if ps.BranchID == nil {
			fmt.Printf("Skipping pageserver at %s: no timeline identity yet\n", ps.DataDir)
			continue
		}
		if err := startPageserver(ctx, env, ps, timeout); err != nil {
			return fmt.Errorf("start pageserver at %s: %w", ps.DataDir, err)
		}
	}
	for i := range env.Safekeepers {
		sk := &env.Safekeepers[i]
		if err := startSafekeeper(env, sk, timeout); err != nil {
			return fmt.Errorf("start safekeeper for endpoint %s: %w", sk.EndpointID, err)
		}
	}
	for i := range env.EndpointStorages {
		es := &env.EndpointStorages[i]
		if !fileExists(env.EndpointStorageBin()) {
			continue
		}
		if err := startEndpointStorage(env, es, timeout); err != nil {
			return fmt.Errorf("start endpoint_storage for endpoint %s: %w", es.EndpointID, err)
		}
	}
	return nil
}

// StopAll stops every endpoint compute, endpoint_storage, pageserver,
// safekeeper and controller instance, best-effort. Mirrors Rust
// `try_stop_all`. Used by the `start` failure-cleanup path.
func StopAll(ctx context.Context, env *config.LocalEnv, immediate bool) {
	mode := StopModeFast
	if immediate {
		mode = StopModeImmediate
	}
	if endpoints, err := loadEndpoints(ctx, env); err == nil {
		for id := range endpoints {
			if err := EndpointStop(ctx, env, id, mode, false); err != nil {
				fmt.Printf("postgres stop failed: %v\n", err)
			}
		}
	} else {
		fmt.Printf("postgres stop failed, could not restore control plane data from env: %v\n", err)
	}

	for i := range env.EndpointStorages {
		if err := stopEndpointStorage(env, &env.EndpointStorages[i], immediate); err != nil {
			fmt.Printf("endpoint_storage for endpoint %s stop failed: %v\n", env.EndpointStorages[i].EndpointID, err)
		}
	}
	for i := range env.Pageservers {
		if err := stopPageserver(&env.Pageservers[i], immediate); err != nil {
			fmt.Printf("pageserver at %s stop failed: %v\n", env.Pageservers[i].DataDir, err)
		}
	}
	for i := range env.Safekeepers {
		if err := stopSafekeeper(env, &env.Safekeepers[i], immediate); err != nil {
			fmt.Printf("safekeeper for endpoint %s stop failed: %v\n", env.Safekeepers[i].EndpointID, err)
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
