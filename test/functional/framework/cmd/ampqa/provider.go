package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/docker"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/exclusivek8s"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/fake"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/universe"
	"github.com/yugabyte/yb-amp/test/functional/suites/canary"
)

// resolveProviderName maps the requested --env-provider (possibly "" / "auto")
// to a concrete provider name, using the same config-driven precedence run uses
// (universe > sharedk8s > exclusivek8s > docker); fakeEnv forces "fake". When
// nothing is configured but a kube-context is discoverable (ambient
// `kubectl config current-context`, apps.ybk8s.kubecontext, or
// env.sharedk8s.context), it defaults to self-building k8s (sharedk8s in deploy
// mode) — so `run --tag X` with only a kube-context stands up its own control
// plane.
func resolveProviderName(name string, cfg *core.Config, fakeEnv bool) string {
	name = strings.ToLower(name)
	if fakeEnv {
		return "fake"
	}
	if name != "" && name != "auto" {
		return name
	}
	switch {
	case universe.FromConfig(cfg).Configured():
		return "universe"
	case sharedk8s.FromConfig(cfg).Configured():
		return "sharedk8s"
	case exclusivek8s.FromConfig(cfg).Configured():
		return "exclusivek8s"
	case docker.FromConfig(cfg).Configured():
		return "docker"
	case discoverKubeContext(cfg) != "":
		// Only an ambient/ybk8s kube-context: self-build on k8s.
		return "sharedk8s"
	default:
		return "none"
	}
}

// discoverKubeContext finds a kube-context to self-build on when none is set
// under env.sharedk8s/env.ampstack: apps.ybk8s.kubecontext, else the ambient
// `kubectl config current-context`. Returns "" when neither is available.
func discoverKubeContext(cfg *core.Config) string {
	if c := cfg.GetString("apps.ybk8s.kubecontext", ""); c != "" {
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildProvider resolves a concrete environment provider by name. It returns
// exactly one of: a live provider; a non-empty unavailableReason (the named
// provider isn't configured — real-env tests skip cleanly); or an error (an
// unknown provider name). Shared by run, setup, and env rm so all three select
// providers identically.
func buildProvider(name string, cfg *core.Config, secrets *core.Secrets, masker *core.Masker, detected core.DetectedVersions) (core.EnvProvider, string, error) {
	switch name {
	case "fake":
		return &fake.Provider{Versions: detected, FailLabelSubstr: canary.EnvFailSubstr}, "", nil
	case "docker":
		if dcfg := docker.FromConfig(cfg); dcfg.Configured() {
			return docker.New(dcfg), "", nil
		}
		return nil, "env.docker not configured", nil
	case "sharedk8s":
		scfg := sharedk8s.FromConfig(cfg)
		// Deploy mode auto-selected off a bare ambient/ybk8s context: inject it so
		// the self-build has a cluster to target.
		if scfg.Deploy && !scfg.Configured() {
			if kc := discoverKubeContext(cfg); kc != "" {
				scfg.Context = kc
				scfg.AmpStack.Context = kc
			}
		}
		if scfg.Configured() {
			return sharedk8s.New(scfg), "", nil
		}
		return nil, "env.sharedk8s not configured (need controller_url or context)", nil
	case "exclusivek8s":
		if xcfg := exclusivek8s.FromConfig(cfg); xcfg.Configured() {
			return exclusivek8s.New(xcfg), "", nil
		}
		return nil, "env.exclusivek8s not configured (need context)", nil
	case "universe":
		if ucfg := universe.FromConfig(cfg); ucfg.Configured() {
			return universe.New(ucfg,
				universe.WithLogf(masker.Logf(log.Printf)),
				universe.WithProvisionYBA(cfg, secrets.Value)), "", nil
		}
		return nil, "env.universe not configured (set env.universe.mode)", nil
	case "none":
		return nil, "no environment provider configured (set env.docker/env.sharedk8s/env.exclusivek8s/env.universe, or use --fake-env)", nil
	default:
		return nil, "", fmt.Errorf("unknown --env-provider %q (want fake|docker|sharedk8s|exclusivek8s|universe|auto)", name)
	}
}
