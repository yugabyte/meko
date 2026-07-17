package main

import (
	"context"
	"fmt"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/reap"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
)

// teardownViaProvider reconstructs an environment from a serialized ref via the
// provider's Attach seam and tears it down — the cross-process reaping path the
// framework's EnvProvider contract exists for. Kept separate so it is unit-
// tested directly against the fake provider.
func teardownViaProvider(ctx context.Context, prov core.EnvProvider, ref string) error {
	env, err := prov.Attach(ctx, ref)
	if err != nil {
		return fmt.Errorf("attach %q: %w", ref, err)
	}
	return prov.Teardown(ctx, env)
}

// ampstackNSPlan reconstructs a KindAmp namespace-reap plan from an ampstack
// standing-env entry that recorded no serialized reap.Plan. The namespace +
// kube context live in the entry Data (recorded at setup time); the reap reuses
// ampstack.Reap's managed-by-guarded delete, so it only removes an ampqa-owned
// namespace. Errors if the entry recorded no namespace to target.
func ampstackNSPlan(entry registry.Entry) (*reap.Plan, error) {
	ns := entry.Data["namespace"]
	if ns == "" {
		return nil, fmt.Errorf("ampstack entry %s has no reap_plan and no recorded namespace; delete the namespace manually, then `env rm --force` to drop the record", entry.ID)
	}
	return &reap.Plan{Namespaces: []reap.NSReap{{
		Kind:        reap.KindAmp,
		Namespace:   ns,
		KubeContext: entry.Data["context"],
	}}}, nil
}

// reapStandingEnv runs a registered standing environment's REAL teardown, in a
// process separate from the one that created it, using only the entry's
// serialized Data. Two mechanisms, in order:
//
//  1. Reap plan (functional-standing preset shapes): when the entry carries a
//     serialized reap.Plan, execute it — the cloud tag-reap and/or the k8s
//     namespace reaps (ybk8s / ybastack / YBA-created universe namespaces) the
//     shape's provisioner recorded, reusing the apps' own teardown code. This is
//     the airtight, closure-free backstop for the preset suites.
//  2. Ownership-tag reap (bare cloud VM shapes): when the entry carries run_tag +
//     a known cloud, terminate every instance of that run tag directly via the
//     cloud — the same guarantee, for older/standalone entries with no plan.
//  3. Namespace reap for ampstack (dev-sandbox / preset control planes): ampstack
//     is NOT an env-provider, so it can't be rebuilt via buildProvider. When such
//     an entry has no serialized plan (a pre-reap-plan setup), reap its recorded
//     namespace directly — the same managed-by-guarded delete ampstack.Reap does.
//  4. Provider Attach + Teardown: rebuild the named provider from config and
//     reap the ref (fake / attach_yba / docker), for shapes whose provider can
//     reconstruct an environment from a ref.
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

	// ampstack standing envs (the dev-sandbox / preset control-plane shape) are
	// not env-providers, so buildProvider rejects the name ("unknown --env-provider
	// ampstack"). Reap the recorded namespace directly instead — ampstack.Reap's
	// managed-by-guarded delete, reconstructed from the entry's namespace+context.
	if entry.Provider == "ampstack" && (providerOverride == "" || providerOverride == "auto") {
		nsPlan, err := ampstackNSPlan(entry)
		if err != nil {
			return err
		}
		return reap.Execute(ctx, nsPlan, cfg, secrets.Value, force)
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
