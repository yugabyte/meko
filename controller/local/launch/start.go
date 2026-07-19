package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// StartFailure signals that `start` failed and the caller should exit(2)
// (matching Rust `handle_start_all`, which stop-alls then `exit(2)`).
type StartFailure struct{ Err error }

func (e *StartFailure) Error() string { return e.Err.Error() }
func (e *StartFailure) Unwrap() error { return e.Err }

// StartAll brings up the whole stack: (1) provision the external database,
// (2) start the controller (waits for its migrations), (3) start the
// provisioned pageservers + safekeepers + endpoint_storages, then poll the
// controller until the expected pageservers are Active. On any failure it
// stop-alls and returns a *StartFailure. Mirrors Rust `handle_start_all` +
// `handle_start_all_impl`.
func StartAll(ctx context.Context, env *config.LocalEnv, timeout time.Duration) error {
	if err := startAllImpl(ctx, env, timeout); err != nil {
		fmt.Println("startup failed because one or more services could not be started")
		fmt.Printf("%v\n", err)
		StopAll(ctx, env, true)
		return &StartFailure{Err: err}
	}
	if err := startStatusCheck(ctx, env, timeout); err != nil {
		return fmt.Errorf("status check after successful startup of all services: %w", err)
	}
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
			fmt.Printf("Skipping pageserver %d: no timeline identity yet\n", ps.ID)
			continue
		}
		if err := startPageserver(ctx, env, ps, timeout); err != nil {
			return fmt.Errorf("start pageserver %d: %w", ps.ID, err)
		}
	}
	for i := range env.Safekeepers {
		sk := &env.Safekeepers[i]
		if err := startSafekeeper(env, sk, timeout); err != nil {
			return fmt.Errorf("start safekeeper %d: %w", sk.ID, err)
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

// startStatusCheck polls the controller's node list until every provisioned
// pageserver (one with a baked identity) is registered and Active. Mirrors
// Rust `yb_amp_start_status_check`.
func startStatusCheck(ctx context.Context, env *config.LocalEnv, timeout time.Duration) error {
	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())

	var expected []config.NodeID
	for _, ps := range env.Pageservers {
		if ps.BranchID != nil {
			expected = append(expected, ps.ID)
		}
	}

	fmt.Println("\nRunning ybAmp status check")
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("status check cancelled: %w", err)
		}
		nodes, err := ctrl.NodeList(ctx)
		if err == nil && nodesActive(nodes, expected) {
			fmt.Println("\nybAmp started and passed status check")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ybAmp did not pass status check within %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// nodesActive reports whether every expected pageserver id is present in nodes
// and Active. Extra registered nodes are tolerated.
func nodesActive(nodes []registry.NodeDescribeResponse, expected []config.NodeID) bool {
	if len(nodes) < len(expected) {
		return false
	}
	byID := map[config.NodeID]string{}
	for _, n := range nodes {
		byID[n.ID] = n.Availability
	}
	for _, id := range expected {
		avail, ok := byID[id]
		if !ok || avail != "Active" {
			return false
		}
	}
	return true
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
			fmt.Printf("pageserver %d stop failed: %v\n", env.Pageservers[i].ID, err)
		}
	}
	for i := range env.Safekeepers {
		if err := stopSafekeeper(env, &env.Safekeepers[i], immediate); err != nil {
			fmt.Printf("safekeeper %d stop failed: %v\n", env.Safekeepers[i].ID, err)
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
