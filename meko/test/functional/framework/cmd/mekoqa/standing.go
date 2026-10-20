package main

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/registry"
)

// recordStandingEnv records sr's standing env in reg. If the registry write
// fails, it runs sr's teardown so the live environment is never orphaned (a
// registry-write failure would otherwise drop the only handle to it), and
// returns the error after teardown ran.
func recordStandingEnv(reg *registry.Registry, sr *core.SetupResult, entry registry.Entry, tdTimeout time.Duration, warnf func(string, ...any)) (registry.Entry, error) {
	stored, err := reg.Add(entry)
	if err == nil {
		return stored, nil
	}
	_ = runSetupTeardown(sr, tdTimeout, warnf)
	return registry.Entry{}, fmt.Errorf("record standing env (env torn down to avoid orphan): %w", err)
}

// runSetupTeardown runs sr's teardown on its own background deadline (the process
// is exiting, so the request context may be cancelled). A teardown failure is
// surfaced via warnf, the infra may survive and must be reaped by tag/plan.
func runSetupTeardown(sr *core.SetupResult, tdTimeout time.Duration, warnf func(string, ...any)) error {
	if sr == nil || sr.Teardown == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tdTimeout)
	defer cancel()
	err := sr.Teardown(ctx)
	if err != nil && warnf != nil {
		warnf("teardown after failed registration also failed (infra may survive; reap by tag/plan): %v", err)
	}
	return err
}

// A kept env always expires. With no expiry the cluster janitor once held
// keep-failed stacks forever and left meko-dev at ~40 nodes. meko-cleaner reaps
// any kept namespace at maxKeepTTL of age on its own.
const (
	defaultKeepTTL = 24 * time.Hour
	maxKeepTTL     = 48 * time.Hour
)

func validateKeepTTL(ttl time.Duration) error {
	if ttl <= 0 || ttl > maxKeepTTL {
		return fmt.Errorf("--ttl %s: want more than 0 and at most %s", ttl, maxKeepTTL)
	}
	return nil
}

// standingEntry builds the registry entry from a StandingEnv, applying the
// --name / --ttl overrides. The connection info plus the reconstruction keys
// (provider name + attach ref) all live in Data so `env rm` can reap it in a
// separate process with only the file.
func standingEntry(env *core.StandingEnv, name string, ttl time.Duration, ttlSet bool) registry.Entry {
	data := map[string]string{}
	for k, v := range env.Info {
		data[k] = v
	}
	if env.Ref != "" {
		data["ref"] = env.Ref
	}
	label := env.Name
	if name != "" {
		label = name
	}
	effTTL := env.TTL
	if ttlSet {
		effTTL = ttl
	}
	if effTTL <= 0 {
		effTTL = defaultKeepTTL
	}
	effTTL = min(effTTL, maxKeepTTL)
	return registry.Entry{
		Name:     label,
		Shape:    env.Shape,
		Provider: env.Provider,
		TTL:      effTTL,
		Data:     data,
	}
}
