package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/config"
)

// `pageserver stop` and its siblings address daemons the endpoint's supervisor
// owns, and it restarts anything signalled behind its back. So each of these is
// a request to the supervisor over the endpoint's control socket.
//
// The supervisor's answer is the whole result. A stop it accepts sticks, because
// it records the service as not wanted; a start it accepts has already passed the
// daemon's health probe.

// daemonStart starts one of an endpoint's daemons, bringing up the endpoint's
// supervisor first if it is not running.
//
// Starting the supervisor starts the whole endpoint, which is what the caller of
// `pageserver start` on a stopped endpoint wants: a pageserver does not run on
// its own, so the alternative is to refuse the command.
func daemonStart(ctx context.Context, env *config.LocalEnv, endpointID, service string, timeout time.Duration) error {
	le, err := resolveEndpoint(ctx, env, endpointID)
	if err != nil {
		return err
	}
	if err := renderDaemonConfig(env, le, service); err != nil {
		return err
	}
	// The supervisor runs every service or none, so it needs the compute's spec
	// even when the caller only asked for a daemon.
	if err := renderComputeSpec(ctx, env, le); err != nil {
		return err
	}
	if err := startEndpointSupervisor(env, le, timeout); err != nil {
		return err
	}
	// A no-op when the supervisor's own startup already started it.
	return supervisorStart(env, le.conf.EndpointID, service, timeout)
}

// daemonStop stops one of an endpoint's daemons and keeps it stopped.
func daemonStop(ctx context.Context, env *config.LocalEnv, endpointID, service string, immediate bool) error {
	le, err := resolveEndpoint(ctx, env, endpointID)
	if err != nil {
		return err
	}
	if !supervisorRunning(env, le.conf.EndpointID) {
		// Same answer proc.StopProcess gives for a missing pidfile: nothing is
		// running, so there is nothing to stop.
		return nil
	}
	return supervisorStop(env, le.conf.EndpointID, "stop", service, immediate)
}

// renderDaemonConfig rewrites the config the launcher owns for the endpoint's
// daemons, before any of them is started.
//
// The launcher renders these files and the supervisor only reads them, so a
// start is the moment to write them: the config can depend on the launcher's
// environment at the time of the command. `REQUIRE_WRITE_TOKEN=false` — the
// fencing escape hatch — has to reach both daemons' own configs this way, or
// they arm themselves from their defaults and refuse every write the unfenced
// supervisor never sends a token for.
func renderDaemonConfig(env *config.LocalEnv, le launcherEndpoint, _ string) error {
	for i := range env.Safekeepers {
		if env.Safekeepers[i].EndpointID == le.conf.EndpointID {
			if err := writeSafekeeperFiles(env, &env.Safekeepers[i]); err != nil {
				return err
			}
			break
		}
	}
	if psConf := env.PageserverConfForBranch(le.conf.BranchID); psConf != nil {
		tomlPath := filepath.Join(psConf.DataDir, "pageserver.toml")
		if err := cfg.SetPageserverFencing(tomlPath, fencingOverride()); err != nil {
			return err
		}
	}
	return nil
}

// fencingOverride reads REQUIRE_WRITE_TOKEN from the launcher's environment:
// nil when unset (the daemons' own default, on, rules), a value when set.
func fencingOverride() *bool {
	v, ok := os.LookupEnv("REQUIRE_WRITE_TOKEN")
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	b := v == "true" || v == "1"
	return &b
}

// daemonStopTimeout bounds a stop request. The supervisor drains the child
// first, so this has to outlast its stop grace rather than race it.
const daemonStopTimeout = 60 * time.Second

// PageserverStart starts the pageserver serving the given endpoint.
func PageserverStart(ctx context.Context, env *config.LocalEnv, endpointID string, timeout time.Duration) error {
	return daemonStart(ctx, env, endpointID, "pageserver", timeout)
}

// PageserverStop stops the pageserver serving the given endpoint.
func PageserverStop(ctx context.Context, env *config.LocalEnv, endpointID string, immediate bool) error {
	return daemonStop(ctx, env, endpointID, "pageserver", immediate)
}

// PageserverRestart stops then starts the pageserver serving the given
// endpoint.
func PageserverRestart(ctx context.Context, env *config.LocalEnv, endpointID string, timeout time.Duration) error {
	if err := PageserverStop(ctx, env, endpointID, false); err != nil {
		return err
	}
	return PageserverStart(ctx, env, endpointID, timeout)
}

// SafekeeperStart starts the safekeeper serving the given endpoint.
func SafekeeperStart(ctx context.Context, env *config.LocalEnv, endpointID string, timeout time.Duration) error {
	return daemonStart(ctx, env, endpointID, "safekeeper", timeout)
}

// SafekeeperStop stops the safekeeper serving the given endpoint.
func SafekeeperStop(ctx context.Context, env *config.LocalEnv, endpointID string, immediate bool) error {
	return daemonStop(ctx, env, endpointID, "safekeeper", immediate)
}

// SafekeeperRestart stops then starts the safekeeper serving the given
// endpoint.
func SafekeeperRestart(ctx context.Context, env *config.LocalEnv, endpointID string, immediate bool, timeout time.Duration) error {
	if err := SafekeeperStop(ctx, env, endpointID, immediate); err != nil {
		return err
	}
	return SafekeeperStart(ctx, env, endpointID, timeout)
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
