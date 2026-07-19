package launch

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/controller/local/config"
)

// PageserverStart starts the pageserver with the given id (register+attach,
// then spawn). Mirrors the `PageserverCmd::Start` arm of Rust
// `handle_pageserver`.
func PageserverStart(ctx context.Context, env *config.LocalEnv, id config.NodeID, timeout time.Duration) error {
	conf, err := env.GetPageserverConf(id)
	if err != nil {
		return err
	}
	return startPageserver(ctx, env, conf, timeout)
}

// PageserverStop stops the pageserver with the given id.
func PageserverStop(env *config.LocalEnv, id config.NodeID, immediate bool) error {
	conf, err := env.GetPageserverConf(id)
	if err != nil {
		return err
	}
	return stopPageserver(conf, immediate)
}

// PageserverRestart stops then starts the pageserver with the given id.
func PageserverRestart(ctx context.Context, env *config.LocalEnv, id config.NodeID, timeout time.Duration) error {
	conf, err := env.GetPageserverConf(id)
	if err != nil {
		return err
	}
	if err := stopPageserver(conf, false); err != nil {
		return err
	}
	return startPageserver(ctx, env, conf, timeout)
}

// SafekeeperStart starts the safekeeper with the given id (write toml, spawn).
// Mirrors the `SafekeeperCmd::Start` arm of Rust `handle_safekeeper`.
func SafekeeperStart(env *config.LocalEnv, id config.NodeID, timeout time.Duration) error {
	for i := range env.Safekeepers {
		if env.Safekeepers[i].ID == id {
			return startSafekeeper(env, &env.Safekeepers[i], timeout)
		}
	}
	return fmt.Errorf("could not find safekeeper %d", id)
}

// SafekeeperStop stops the safekeeper with the given id.
func SafekeeperStop(env *config.LocalEnv, id config.NodeID, immediate bool) error {
	for i := range env.Safekeepers {
		if env.Safekeepers[i].ID == id {
			return stopSafekeeper(env, &env.Safekeepers[i], immediate)
		}
	}
	return fmt.Errorf("could not find safekeeper %d", id)
}

// SafekeeperRestart stops then starts the safekeeper with the given id.
func SafekeeperRestart(env *config.LocalEnv, id config.NodeID, immediate bool, timeout time.Duration) error {
	for i := range env.Safekeepers {
		if env.Safekeepers[i].ID == id {
			if err := stopSafekeeper(env, &env.Safekeepers[i], immediate); err != nil {
				return err
			}
			return startSafekeeper(env, &env.Safekeepers[i], timeout)
		}
	}
	return fmt.Errorf("could not find safekeeper %d", id)
}

// EndpointStorageStart starts every provisioned endpoint_storage instance.
// Mirrors the `Start` arm of Rust `handle_endpoint_storage`.
func EndpointStorageStart(env *config.LocalEnv, timeout time.Duration) error {
	for i := range env.EndpointStorages {
		if !fileExists(env.EndpointStorageBin()) {
			fmt.Printf("%s binary not found. Ignore if this is a compatibility test\n", env.EndpointStorageBin())
			return nil
		}
		if err := startEndpointStorage(env, &env.EndpointStorages[i], timeout); err != nil {
			return err
		}
	}
	return nil
}

// ControllerStart starts a controller instance (external-YB path). Mirrors the
// `ControllerCmd::Start` arm of Rust `handle_controller`.
func ControllerStart(ctx context.Context, env *config.LocalEnv, instanceID uint8, timeout time.Duration) error {
	return startController(ctx, env, instanceID, timeout)
}

// ControllerStop stops a controller instance.
func ControllerStop(env *config.LocalEnv, instanceID uint8, immediate bool) error {
	return stopController(env, instanceID, immediate)
}
