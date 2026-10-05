package main

import (
	"context"
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cloud"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/registry"
)

// teardownViaProvider reconstructs an environment from a serialized ref via the
// provider's Attach seam and tears it down, the cross-process reaping path the
// framework's EnvProvider contract exists for. Kept separate so it is unit-
// tested directly against the fake provider.
func teardownViaProvider(ctx context.Context, prov core.EnvProvider, ref string) error {
	env, err := prov.Attach(ctx, ref)
	if err != nil {
		return fmt.Errorf("attach %q: %w", ref, err)
	}
	return prov.Teardown(ctx, env)
}

// reapStandingEnv runs a registered standing environment's REAL teardown, in a
// process separate from the one that created it, using only the entry's
// serialized Data. Two mechanisms, in order:
//
//  1. Reap plan: when the entry carries a
//     serialized reap.Plan, execute it, the cloud tag-reap and/or the k8s
//     namespace reaps (ybk8s / ybastack / YBA-created universe namespaces) the
//     shape's provisioner recorded, reusing the apps' own teardown code.
//  2. Ownership-tag reap (bare cloud VM shapes): when the entry carries run_tag +
//     a known cloud, terminate every instance of that run tag directly via the
//     cloud, the same guarantee, for older/standalone entries with no plan.
//  3. Provider Attach + Teardown: rebuild the named provider from config and
//     reap the ref, for shapes whose provider can reconstruct an environment
//     from a ref.
func reapStandingEnv(ctx context.Context, entry registry.Entry, cfg *core.Config, secrets *core.Secrets, masker *core.Masker, providerOverride string, detected core.DetectedVersions, force bool) error {
	plan, err := reap.FromJSON(entry.Data["reap_plan"])
	if err != nil {
		return err
	}
	if !plan.Empty() {
		return reap.Execute(ctx, plan, cfg, secrets.Value, force)
	}

	if tag := entry.Data["run_tag"]; tag != "" {
		if cl := entry.Data["cloud"]; cl == "aws" || cl == "gcp" {
			vm, err := cloud.New(cl, cfg, secrets.Value)
			if err != nil {
				return fmt.Errorf("build %s client for tag reap: %w", cl, err)
			}
			return vm.ReapByTag(ctx, tag)
		}
	}

	name := entry.Provider
	if providerOverride != "" && providerOverride != "auto" {
		name = providerOverride
	}
	if name == "" {
		return fmt.Errorf("entry has no provider and no --env-provider given; cannot reconstruct teardown")
	}
	prov, unavailable, err := buildProvider(name, cfg, secrets, masker, detected)
	if err != nil {
		return err
	}
	if prov == nil {
		return fmt.Errorf("cannot reconstruct provider %q for teardown: %s", name, unavailable)
	}
	return teardownViaProvider(ctx, prov, entry.Data["ref"])
}
