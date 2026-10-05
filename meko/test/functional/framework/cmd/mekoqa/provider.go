package main

import (
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	k8senv "github.com/yugabyte/meko/meko/test/functional/framework/env/k8s"
)

// resolveProviderName maps the requested --env-provider (possibly "" / "auto")
// to a concrete provider name. fakeEnv forces "fake". With nothing requested, a
// configured or ambient kube-context selects "k8s" and anything else "none".
func resolveProviderName(name string, cfg *core.Config, fakeEnv bool) string {
	name = strings.ToLower(name)
	if fakeEnv {
		return "fake"
	}
	if name != "" && name != "auto" {
		return name
	}
	if discoverKubeContext(cfg) != "" {
		return "k8s"
	}
	return "none"
}

// discoverKubeContext finds the kube-context to run on: --kube-context (which run
// folds into env.k8s.context), else the ambient `kubectl config current-context`.
// Returns "" when neither is set.
func discoverKubeContext(cfg *core.Config) string { return k8senv.DiscoverContext(cfg) }

// buildProvider resolves a concrete environment provider by name. It returns
// exactly one of: a live provider; a non-empty unavailableReason (the named
// provider isn't available, so real-env tests skip cleanly); or an error (an
// unknown provider name). Shared by run, setup, and env rm so all three select
// providers identically.
func buildProvider(name string, cfg *core.Config, secrets *core.Secrets, masker *core.Masker, detected core.DetectedVersions) (core.EnvProvider, string, error) {
	switch name {
	case "fake":
		return &fake.Provider{Versions: detected, FailLabelSubstr: fake.EnvFailSubstr}, "", nil
	case "k8s":
		kc, err := k8senv.FromConfig(cfg, secrets.Get)
		if err != nil {
			return nil, "", fmt.Errorf("env provider k8s: %w", err)
		}
		if kc.Context = discoverKubeContext(cfg); kc.Context == "" {
			return nil, "no kube context (pass --kube-context, set env.k8s.context, or select one with kubectl)", nil
		}
		return k8senv.New(kc), "", nil
	case "none":
		return nil, "no environment provider configured (set env.k8s.context, or use --fake-env)", nil
	default:
		return nil, "", fmt.Errorf("unknown --env-provider %q (want fake|k8s|auto)", name)
	}
}
