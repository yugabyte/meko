package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/sched"
)

// newSetupCmd builds `ampqa setup`: run ONLY a test's setup phase (provision +
// prepare fixtures), leave the environment UP, register it in the standing-env
// registry, and print its connection info. It never runs the test body or
// teardown. `ampqa env ls` then lists it and `ampqa env rm <id>` tears it down.
func newSetupCmd() *cobra.Command {
	var (
		suiteRe     string
		testName    string
		testRegex   string
		tags        []string
		seed        int64
		configFiles []string
		jsonConfigs []string
		overrides   []string
		envProvider string
		fakeEnv     bool
		dryRun      bool
		name        string
		ttl         time.Duration
		regPath     string
		noDefaults  bool
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Stand a test's environment up and leave it running for manual use (DEPRECATED: use `run --setup-only`)",
		Long: `Run ONLY a test's setup phase — provision its environment and prepare
fixtures — then STOP, leaving the environment up and registered in the standing
-environment registry. The test body and its assertions are NOT run, and the
environment is NOT torn down. Select exactly one setup-capable test with
--suite/--test/--tag (the preset suite offers ready-made menus, e.g.
'ampqa setup -t presets/yb_k8s'). List standing environments with 'ampqa env
ls' and tear one down with 'ampqa env rm <id>'.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if !jsonOutput {
				fmt.Fprintln(c.ErrOrStderr(), "note: `ampqa setup` is deprecated — use `ampqa run --setup-only --test <name>` (same engine).")
			}
			loadConfig := core.LoadConfig
			if noDefaults {
				loadConfig = core.LoadConfigNoDefaults
			}
			cfg, err := loadConfig(configFiles, jsonConfigs, overrides)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			secrets, err := core.NewSecrets(cfg)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			masker := core.NewMaskerFromSecrets(secrets)

			testRe, terr := resolveTestFilter(testName, testRegex)
			if terr != nil {
				return &exitError{code: 2, err: terr}
			}
			test, err := selectOneTest(suiteRe, testRe, tags)
			if err != nil {
				return err
			}

			runSeed := core.NewRunSeed(seed, c.Flags().Changed("seed"))
			detected := core.DetectedVersions{
				Product: cfg.GetString("versions.product", ""),
				YB:      cfg.GetString("versions.yb", ""),
			}
			opts := core.RunOptions{
				Seed:             runSeed,
				Config:           cfg,
				DetectedVersions: detected,
				Masker:           masker,
			}
			// Selecting a provider mirrors run's flags; a self-provisioning setup
			// (the presets) ignores it, but resolving it surfaces an unknown name
			// as a usage error and keeps the CLI surface consistent with run.
			provName := resolveProviderName(envProvider, cfg, fakeEnv || dryRun)
			if _, _, perr := buildProvider(provName, cfg, secrets, masker, detected); perr != nil {
				return &exitError{code: 2, err: perr}
			}

			// PERSISTENT lease: reserve capacity for the standing env and pin it so
			// the pool never reclaims the slot. The process exits after setup, so
			// this bounds concurrent setups within one invocation and records the
			// pin intent; the registry entry (no auto-teardown, TTL below) is what
			// keeps the env alive across invocations until `env rm`.
			mgr := sched.NewManager(cfg.GetInt("sched.capacity", sched.DefaultCapacity))
			weight := test.Env.Weight
			if weight < 1 {
				weight = 1
			}
			if lease, lerr := mgr.Acquire(c.Context(), weight); lerr == nil {
				lease.Pin()
			}

			result, sr := core.SetupOne(c.Context(), test, opts)
			if result.Status != core.StatusPassed || sr == nil || sr.Env == nil {
				return reportSetupFailure(c, test.Name, result)
			}

			// The environment is now UP and sr.Teardown is the only handle to it
			// until it is recorded in the registry. If recording fails, we MUST run
			// that teardown before returning — dropping the closure would orphan
			// live infrastructure with no record to reap it by.
			tdTimeout := core.ResolveTeardownTimeout(cfg)
			warnf := func(format string, args ...any) { fmt.Fprintf(c.ErrOrStderr(), format+"\n", args...) }

			reg, err := openRegistry(regPath)
			if err != nil {
				_ = runSetupTeardown(sr, tdTimeout, warnf)
				return &exitError{code: 1, err: fmt.Errorf("open registry (env torn down to avoid orphan): %w", err)}
			}
			entry := standingEntry(sr.Env, name, ttl, c.Flags().Changed("ttl"))
			stored, err := recordStandingEnv(reg, sr, entry, tdTimeout, warnf)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			printSetupOK(c, test.Name, stored)
			return nil
		},
	}
	cmd.Flags().StringVar(&suiteRe, "suite", "", "suite name regex")
	cmd.Flags().StringVarP(&testName, "test", "t", "", "exact test name (must resolve to exactly one test); use --testr for a regex")
	cmd.Flags().StringVar(&testRegex, "testr", "", "test name regex (mutually exclusive with --test)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "require tag (repeatable)")
	cmd.Flags().Int64Var(&seed, "seed", 0, "run seed (default: random)")
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().StringVar(&envProvider, "env-provider", "auto", "environment provider: fake|docker|sharedk8s|exclusivek8s|universe|auto")
	cmd.Flags().BoolVar(&fakeEnv, "fake-env", false, "use the fake env provider — NOTE: self-provisioning tests still stand up real infra in their body")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "DEPRECATED alias of --fake-env")
	_ = cmd.Flags().MarkDeprecated("dry-run", "use --fake-env instead")
	cmd.Flags().StringVar(&name, "name", "", "label for the standing environment (default: the test's own)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "auto-reap the standing env after this long (default 0: never — reap explicitly with env rm)")
	cmd.Flags().StringVar(&regPath, "registry", "", "registry file path (default ~/.ampqa/envs.json)")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer (debugging only)")
	return cmd
}

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
// surfaced via warnf — the infra may survive and must be reaped by tag/plan.
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

// selectOneTest resolves the filter to exactly one test, erroring clearly when
// none or several match — a setup invocation stands up one environment.
func selectOneTest(suiteRe, testRe string, tags []string) (core.Test, error) {
	tests, err := core.Default().Filter(suiteRe, testRe, tags)
	if err != nil {
		return core.Test{}, &exitError{code: 2, err: err}
	}
	if len(tests) == 0 {
		return core.Test{}, &exitError{code: 3, err: fmt.Errorf("no tests matched suite=%q test=%q tags=%v", suiteRe, testRe, tags)}
	}
	if len(tests) > 1 {
		names := make([]string, len(tests))
		for i, t := range tests {
			names[i] = t.Name
		}
		sort.Strings(names)
		return core.Test{}, &exitError{code: 2, err: fmt.Errorf("%d tests matched; setup needs exactly one — narrow with --test: %v", len(tests), names)}
	}
	return tests[0], nil
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
	return registry.Entry{
		Name:     label,
		Shape:    env.Shape,
		Provider: env.Provider,
		TTL:      effTTL,
		Data:     data,
	}
}

func reportSetupFailure(c *cobra.Command, testName string, result *core.Result) error {
	if jsonOutput {
		_ = printEnvelope(c.OutOrStdout(), "ampqa.setup", "setup", map[string]any{
			"test":          testName,
			"status":        string(result.Status),
			"error_summary": result.ErrorSummary,
			"skip_reason":   result.SkipReason,
		})
	} else {
		msg := result.ErrorSummary
		if msg == "" {
			msg = result.SkipReason
		}
		fmt.Fprintf(c.OutOrStdout(), "setup %s: %s — %s\n", testName, result.Status, msg)
	}
	if result.Status == core.StatusSkipped {
		return &exitError{code: 0}
	}
	return &exitError{code: 1}
}

func printSetupOK(c *cobra.Command, testName string, e registry.Entry) {
	if jsonOutput {
		_ = printEnvelope(c.OutOrStdout(), "ampqa.setup", "setup", map[string]any{
			"test": testName,
			"env":  envRow(e),
		})
		return
	}
	out := c.OutOrStdout()
	fmt.Fprintf(out, "standing environment up: %s (%s)\n", e.ID, e.Name)
	fmt.Fprintf(out, "  shape=%s provider=%s ttl=%s\n", e.Shape, e.Provider, e.TTL)
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "  %s=%s\n", k, e.Data[k])
	}
	fmt.Fprintf(out, "tear it down with: ampqa env rm %s\n", e.ID)
}
