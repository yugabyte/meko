package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
)

// Environment lifecycle modes for `run --env-mode`.
const (
	envModeBuild  = "build"  // provision, run, tear down (default, today's behaviour)
	envModeAttach = "attach" // attach to --env <id>, run, leave it standing
	// keep-failed: tear down on a fully green run, keep+record the env (like
	// keep) when anything failed — post-mortem access without leaking green envs.
	envModeKeepFailed = "keep-failed"
	envModeKeep       = "keep" // provision, run, RECORD the built env, skip teardown
)

// standingProvider is the optional seam a provider implements to expose the
// run-scoped environment it OWNS as a recordable standing environment (deploy
// mode of sharedk8s). --env-mode keep / --setup-only record it and skip
// teardown; a later `run --env <id>` attaches to it.
type standingProvider interface {
	StandingEnv() (*core.StandingEnv, bool)
}

type closer interface{ Close() error }

// resolveEnvMode validates and reconciles --env-mode / --setup-only / --env into
// a single lifecycle mode. --setup-only forces keep; --env forces attach. The
// provider/fake-env conflicts of --env are enforced by run's existing --env
// block, so they are not re-checked here.
func resolveEnvMode(mode string, setupOnly bool, envID string, modeSet bool) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = envModeBuild
	}
	switch mode {
	case envModeBuild, envModeAttach, envModeKeep, envModeKeepFailed:
	default:
		return "", fmt.Errorf("invalid --env-mode %q (want build|attach|keep|keep-failed)", mode)
	}
	if setupOnly {
		if modeSet && mode != envModeKeep {
			return "", fmt.Errorf("--setup-only implies --env-mode keep; do not also pass --env-mode %s", mode)
		}
		if envID != "" {
			return "", fmt.Errorf("--setup-only stands a NEW environment up; it conflicts with --env (attach to an existing one)")
		}
		return envModeKeep, nil
	}
	if envID != "" {
		if modeSet && mode != envModeAttach {
			return "", fmt.Errorf("--env <id> attaches to a standing environment (mode=attach); do not also pass --env-mode %s", mode)
		}
		return envModeAttach, nil
	}
	if mode == envModeAttach {
		return "", fmt.Errorf("--env-mode attach requires --env <id> (the standing environment to attach to)")
	}
	return mode, nil
}

// keepAndRecord records the provider's owned control plane in the registry and
// SKIPS teardown, so a later `run --env <id>` attaches to it. When the provider
// has nothing standing to keep (attach mode, fake env), it tears down normally.
// On any registry failure it tears the built env down rather than orphan it.
func keepAndRecord(c *cobra.Command, provider core.EnvProvider, cfg *core.Config, regPath, name string, ttl time.Duration, ttlSet bool) error {
	sp, ok := provider.(standingProvider)
	if !ok {
		closeProvider(provider)
		fmt.Fprintln(c.ErrOrStderr(), "warning: --env-mode keep: this provider has no standing environment to record; environment torn down")
		return nil
	}
	se, ok := sp.StandingEnv()
	if !ok {
		closeProvider(provider)
		fmt.Fprintln(c.ErrOrStderr(), "warning: --env-mode keep: no environment was built to record; nothing kept")
		return nil
	}
	reg, err := openRegistry(regPath)
	if err != nil {
		closeProvider(provider)
		return fmt.Errorf("open registry (env torn down to avoid orphan): %w", err)
	}
	stored, err := reg.Add(standingEntry(se, name, ttl, ttlSet))
	if err != nil {
		closeProvider(provider)
		return fmt.Errorf("record standing env (env torn down to avoid orphan): %w", err)
	}
	printKeptEnv(c, stored)
	return nil
}

// runSetupOnly stands each selected test's environment up, SKIPS its body, and
// leaves the environment running and recorded — the "set up the env a test
// needs" path, folding the deprecated `ampqa setup`. Two kinds of test:
//   - setup-capable (presets, Setup!=nil): self-provision via core.SetupOne and
//     record the StandingEnv they return (the existing machinery).
//   - ordinary tests: the environment they need is the provider's own control
//     plane (already Prepared above); record it once and keep it.
func runSetupOnly(c *cobra.Command, tests []core.Test, provider core.EnvProvider, unavailable string, opts core.RunOptions, cfg *core.Config, regPath, name string, ttl time.Duration, ttlSet bool) error {
	reg, err := openRegistry(regPath)
	if err != nil {
		closeProvider(provider)
		return &exitError{code: 1, err: fmt.Errorf("open registry (env torn down to avoid orphan): %w", err)}
	}
	tdTimeout := core.ResolveTeardownTimeout(cfg)
	warnf := func(format string, args ...any) { fmt.Fprintf(c.ErrOrStderr(), format+"\n", args...) }

	var recorded []registry.Entry
	providerEnvKept := false
	var firstErr error

	for _, test := range tests {
		if test.Setup != nil {
			result, sr := core.SetupOne(c.Context(), test, opts)
			if result.Status != core.StatusPassed || sr == nil || sr.Env == nil {
				warnf("setup-only %s: %s — %s", test.Name, result.Status, firstNonEmpty(result.ErrorSummary, result.SkipReason))
				if result.Status != core.StatusSkipped {
					firstErr = fmt.Errorf("setup-only %s: %s", test.Name, result.Status)
				}
				continue
			}
			stored, e := recordStandingEnv(reg, sr, standingEntry(sr.Env, name, ttl, ttlSet), tdTimeout, warnf)
			if e != nil {
				firstErr = e
				continue
			}
			recorded = append(recorded, stored)
			continue
		}

		// Ordinary test: needs the provider control plane.
		if provider == nil {
			reason := unavailable
			if reason == "" {
				reason = "no environment provider configured"
			}
			warnf("setup-only %s: skipped — %s", test.Name, reason)
			continue
		}
		if providerEnvKept {
			continue // one control plane serves every ordinary test in this selection
		}
		sp, ok := provider.(standingProvider)
		if !ok {
			firstErr = fmt.Errorf("provider cannot stand a reusable environment up for setup-only (%T)", provider)
			continue
		}
		se, ok := sp.StandingEnv()
		if !ok {
			firstErr = fmt.Errorf("provider built no standing environment to record for setup-only")
			continue
		}
		stored, e := reg.Add(standingEntry(se, name, ttl, ttlSet))
		if e != nil {
			closeProvider(provider)
			firstErr = fmt.Errorf("record standing env (env torn down to avoid orphan): %w", e)
			continue
		}
		recorded = append(recorded, stored)
		providerEnvKept = true
	}

	for _, e := range recorded {
		printKeptEnv(c, e)
	}
	// If nothing kept the provider's control plane (all presets, or none matched),
	// it is not part of a recorded env — tear it down so it does not leak.
	if !providerEnvKept {
		closeProvider(provider)
	}
	if firstErr != nil {
		return &exitError{code: 1, err: firstErr}
	}
	if len(recorded) == 0 {
		return &exitError{code: exitAllSkipped}
	}
	return nil
}

// printKeptEnv prints the recorded standing environment and how to reuse/reap it.
func printKeptEnv(c *cobra.Command, e registry.Entry) {
	out := c.OutOrStdout()
	fmt.Fprintf(out, "standing environment recorded: %s (%s)\n", e.ID, e.Name)
	fmt.Fprintf(out, "  shape=%s provider=%s ttl=%s\n", e.Shape, e.Provider, e.TTL)
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "  %s=%s\n", k, e.Data[k])
	}
	fmt.Fprintf(out, "reuse it:   ampqa run --env %s ...\n", e.ID)
	fmt.Fprintf(out, "tear down:  ampqa env rm %s\n", e.ID)
}

func closeProvider(provider core.EnvProvider) {
	if cl, ok := provider.(closer); ok {
		_ = cl.Close()
	}
}
