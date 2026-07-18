package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/report"
	"github.com/yugabyte/amp/test/functional/framework/report/html"
	"github.com/yugabyte/amp/test/functional/framework/report/jsonfile"
	"github.com/yugabyte/amp/test/functional/framework/report/junit"
	"github.com/yugabyte/amp/test/functional/framework/report/portal"
	"github.com/yugabyte/amp/test/functional/framework/report/stdout"
	"github.com/yugabyte/amp/test/functional/framework/sched"
)

func newRunCmd() *cobra.Command {
	var (
		suiteRe     string
		testName    string
		testRegex   string
		tags        []string
		parallel    int
		perEnv      int
		seed        int64
		configFiles []string
		jsonConfigs []string
		overrides   []string
		fakeEnv     bool
		dryRun      bool
		outDir      string
		junitPath   string
		htmlPath    string
		portalURL   string
		reportName  string
		envProvider string
		envID       string
		regPath     string
		noDefaults  bool
		envMode     string
		setupOnly   bool
		envName     string
		envTTL      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run tests through the packing scheduler",
		Long: `Run tests through the packing scheduler.

Selecting tests: --test <exact-name> (e.g. perf/pgbench) | --testr <regex> |
--suite <regex> | --tag <tag> (repeatable). To see what's available and how to
configure it:
  ampqa collect            list every test with a one-line description
  ampqa collect --tag X    list a tier (e.g. --tag regression)
  ampqa describe <test>    a test's full description + parameters (the global run
                           flags below, plus that test's own config knobs)
  ampqa manifest           the machine-readable catalog (commands, tests, config)

Configuration precedence (later wins): the embedded configs/default.yaml
base layer  →  -c files, deep-merged in the order given  →  -jc JSON files,
merged on top  →  -o typed key=value overrides. --no-default-config skips
the embedded layer. --parallel and --per-env-parallelism take their default
from sched.parallel/sched.per_env_parallelism in that merged config when not
passed explicitly; an explicit flag always wins. --out-dir/--junit/--html work
the same way against report.out_dir/report.junit_path/report.html_path.

--env <id> attaches this run to a standing environment (from 'ampqa setup' /
'ampqa env ls'): the registry entry is resolved to its provider and connection
info, so the selected tests target the STANDING control plane / universe instead
of provisioning fresh ones. It forces the matching provider and conflicts with
--fake-env and --env-provider.

Exit codes: 0 all selected tests passed (or passed+skipped); 1 a test
failed/broke/env-failed/timed-out; 2 usage; 3 the selection matched no tests;
4 tests were selected but NONE executed (all skipped — e.g. no provider
available), distinct from a genuine all-pass.`,
		RunE: func(c *cobra.Command, _ []string) error {
			// Environment lifecycle: --env-mode build|attach|keep + --setup-only.
			// build (default): provision, run, tear down. keep: provision, run,
			// RECORD the built env, skip teardown. attach: attach to --env <id>,
			// run, skip teardown. --setup-only: provision + setup fixtures, SKIP the
			// body, force keep. `ampqa setup` is a thin deprecated alias of this.
			mode, merr := resolveEnvMode(envMode, setupOnly, envID, c.Flags().Changed("env-mode"))
			if merr != nil {
				return &exitError{code: 2, err: merr}
			}
			// --env attaches to a standing environment: resolve the registry entry to
			// the matching provider + connection overrides BEFORE loading config, so
			// the derived env.sharedk8s.* / env.universe.* keys layer in as normal
			// overrides (after any user -o) and the provider is selected from the
			// standing env rather than provisioning a fresh one.
			if envID != "" {
				if fakeEnv || dryRun {
					return &exitError{code: 2, err: fmt.Errorf("--env attaches to a standing environment; it conflicts with --fake-env/--dry-run")}
				}
				if c.Flags().Changed("env-provider") {
					return &exitError{code: 2, err: fmt.Errorf("--env selects the provider from the standing environment; do not also pass --env-provider")}
				}
				reg, err := openRegistry(regPath)
				if err != nil {
					return &exitError{code: 1, err: err}
				}
				entry, ok, err := reg.Get(envID)
				if err != nil {
					return &exitError{code: 1, err: err}
				}
				if !ok {
					return &exitError{code: 3, err: fmt.Errorf("env %s not found", envID)}
				}
				provName, attachOverrides, aerr := attachFromEntry(entry)
				if aerr != nil {
					return &exitError{code: 2, err: aerr}
				}
				overrides = append(overrides, attachOverrides...)
				envProvider = provName
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
			if !c.Flags().Changed("parallel") {
				parallel = cfg.GetInt("sched.parallel", parallel)
			}
			if !c.Flags().Changed("per-env-parallelism") {
				perEnv = cfg.GetInt("sched.per_env_parallelism", perEnv)
			}
			if !c.Flags().Changed("out-dir") {
				outDir = cfg.GetString("report.out_dir", outDir)
			}
			if !c.Flags().Changed("junit") {
				junitPath = cfg.GetString("report.junit_path", junitPath)
			}
			if !c.Flags().Changed("portal") {
				portalURL = cfg.GetString("report.portal.url", portalURL)
			}
			if !c.Flags().Changed("report") {
				reportName = cfg.GetString("report.portal.report", reportName)
			}
			if !c.Flags().Changed("html") {
				htmlPath = cfg.GetString("report.html_path", htmlPath)
			}
			testRe, terr := resolveTestFilter(testName, testRegex)
			if terr != nil {
				return &exitError{code: 2, err: terr}
			}
			tests, err := core.Default().Filter(suiteRe, testRe, tags)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			// The canary suite is the framework's self-test; keep it out of
			// normal selection unless --suite was given explicitly.
			if !c.Flags().Changed("suite") {
				tests = excludeSuite(tests, "canary")
			}
			// Setup-only tests (Run==nil, e.g. the presets) have no body to execute
			// in a normal run, so drop them — UNLESS --setup-only was requested,
			// where standing an env up (no body) is the whole point.
			if !setupOnly {
				tests = excludeSetupOnly(tests)
			}
			if len(tests) == 0 {
				return &exitError{code: 3, err: fmt.Errorf("no tests matched suite=%q test=%q tags=%v", suiteRe, testRe, tags)}
			}

			runSeed := core.NewRunSeed(seed, c.Flags().Changed("seed"))
			detected := core.DetectedVersions{
				Product: cfg.GetString("versions.product", ""),
				YB:      cfg.GetString("versions.yb", ""),
			}

			opts := sched.Options{
				RunOptions: core.RunOptions{
					Parallel:         parallel,
					Seed:             runSeed,
					Config:           cfg,
					DetectedVersions: detected,
					Masker:           masker,
				},
				Capacity:          cfg.GetInt("sched.capacity", sched.DefaultCapacity),
				PerEnvParallelism: perEnv,
			}

			// Provider selection. --env-provider forces one; otherwise auto picks by
			// config with precedence universe > sharedk8s > exclusivek8s > docker,
			// and — when only a kube-context is discoverable — self-building k8s
			// (sharedk8s deploy mode). --fake-env always means the fake provider (no
			// infrastructure). A configured-but-unavailable provider leaves
			// ProviderUnavailableReason set, so real-env tests skip cleanly.
			name := resolveProviderName(envProvider, cfg, fakeEnv || dryRun)
			provider, unavailable, perr := buildProvider(name, cfg, secrets, masker, detected)
			if perr != nil {
				return &exitError{code: 2, err: perr}
			}
			opts.Provider = provider
			opts.ProviderUnavailableReason = unavailable

			// Self-building providers (sharedk8s deploy mode) stand their
			// run-scoped control plane up ONCE here, before the scheduler starts —
			// otherwise the first test's short provision deadline would have to
			// absorb the whole control-plane bring-up. A failed bring-up tears down
			// what it created and fails the run.
			if prep, ok := provider.(interface {
				Prepare(context.Context) error
			}); ok {
				if err := prep.Prepare(c.Context()); err != nil {
					if closer, ok := provider.(interface{ Close() error }); ok {
						_ = closer.Close()
					}
					return &exitError{code: 1, err: fmt.Errorf("prepare environment: %w", err)}
				}
			}

			// --setup-only: stand each selected test's environment up, skip its
			// body, leave it running and recorded. One engine with `ampqa setup`.
			if setupOnly {
				return runSetupOnly(c, tests, provider, unavailable, opts.RunOptions, cfg, regPath, envName, envTTL, c.Flags().Changed("ttl"))
			}

			// The result-persisting sinks (jsonfile, junit) are CRITICAL: a test's
			// authoritative pass/fail record must never be dropped under
			// backpressure, or an agent reading a "green" run could be misled when a
			// failing result was silently lost. The live stdout progress sink is
			// non-critical — cosmetic, safe to drop under load — and any such drop is
			// surfaced via Reporter.Dropped() below.
			var progress core.Sink
			if !jsonOutput {
				progress = stdout.New()
			}
			// The portal sink streams the run live to the amp report portal. It
			// is non-critical by design: the portal being down must never fail
			// or slow a run.
			var liveSinks []core.Sink
			if portalURL != "" {
				liveSinks = append(liveSinks, portal.New(portalURL, portal.Meta{
					RunID:       cfg.GetString("report.portal.run_id", ""),
					Report:      reportName,
					Branch:      cfg.GetString("report.portal.branch", ""),
					CommitSHA:   cfg.GetString("report.portal.commit", ""),
					BuildTag:    cfg.GetString("report.portal.build_tag", ""),
					TriggerType: cfg.GetString("report.portal.trigger_type", ""),
					TriggeredBy: cfg.GetString("report.portal.triggered_by", ""),
				}))
			}
			var resultSinks []core.Sink
			if outDir != "" {
				resultSinks = append(resultSinks, jsonfile.New(outDir))
			}
			if junitPath != "" {
				resultSinks = append(resultSinks, junit.New(junitPath))
			}
			// The HTML sink accumulates results in memory and renders a
			// self-contained report once on RunFinished — the same output as
			// `ampqa report --from`, produced on the spot during the run.
			if htmlPath != "" {
				resultSinks = append(resultSinks, html.New(htmlPath))
			}
			var reporter *report.Reporter
			if progress != nil || len(liveSinks) > 0 || len(resultSinks) > 0 {
				reporter = buildReporterWithLive(report.DefaultQueueSize, progress, liveSinks, resultSinks...)
				opts.Sink = reporter
			}

			// c.Context() is the signal-cancelled root context: on SIGINT/SIGTERM
			// the scheduler stops taking new work and in-flight tests run their
			// (background-deadline) teardown before Run returns — no orphaned infra.
			summary, results := sched.Run(c.Context(), tests, opts)
			// Teardown ownership: build/attach close the provider (build tears the
			// owned control plane down; attach only frees the local forward, leaving
			// the standing env intact). keep RECORDS the built control plane and
			// SKIPS teardown so a later `run --env <id>` attaches to it.
			keepNow := mode == envModeKeep ||
				(mode == envModeKeepFailed && summaryHasFailures(summary))
			if keepNow {
				if err := keepAndRecord(c, opts.Provider, cfg, regPath, envName, envTTL, c.Flags().Changed("ttl")); err != nil {
					return &exitError{code: 1, err: err}
				}
			} else if closer, ok := opts.Provider.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
			var reporterDropped int64
			if reporter != nil {
				reporter.Close()
				reporterDropped = reporter.Dropped()
				if reporterDropped > 0 {
					fmt.Fprintf(c.ErrOrStderr(), "warning: %d live-progress report event(s) dropped under backpressure (pass/fail records persist on critical sinks and were not dropped)\n", reporterDropped)
				}
			}

			if jsonOutput {
				_ = printEnvelope(c.OutOrStdout(), "ampqa.run", "run", runData(runSeed.Value(), parallel, summary, results, reporterDropped))
			}

			if code := runExitCode(summary); code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&suiteRe, "suite", "", "suite name regex")
	cmd.Flags().StringVar(&testName, "test", "", "exact test name (e.g. perf/pgbench); use --testr for a regex")
	cmd.Flags().StringVar(&testRegex, "testr", "", "test name regex (mutually exclusive with --test)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "require tag (repeatable)")
	cmd.Flags().IntVar(&parallel, "parallel", 1, "max concurrent test bodies; default from sched.parallel unless passed explicitly")
	cmd.Flags().IntVar(&perEnv, "per-env-parallelism", sched.DefaultPerEnvParallelism, "co-tenant tests per shared environment; default from sched.per_env_parallelism unless passed explicitly")
	cmd.Flags().Int64Var(&seed, "seed", 0, "run seed (default: random)")
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().BoolVar(&fakeEnv, "fake-env", false, "use the fake env provider — NOTE: self-provisioning tests still stand up real infra in their body")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "DEPRECATED alias of --fake-env")
	_ = cmd.Flags().MarkDeprecated("dry-run", "use --fake-env instead")
	cmd.Flags().StringVar(&envProvider, "env-provider", "auto", "environment provider: fake|docker|sharedk8s|exclusivek8s|universe|auto")
	cmd.Flags().StringVar(&envID, "env", "", "attach to a standing environment from the registry by id (forces the matching provider; conflicts with --fake-env/--env-provider)")
	cmd.Flags().StringVar(&envMode, "env-mode", "build", "environment lifecycle: build (provision, run, tear down) | keep (provision, run, RECORD the env, skip teardown) | keep-failed (tear down when green, keep+record when anything failed) | attach (attach to --env, run, skip teardown)")
	cmd.Flags().BoolVar(&setupOnly, "setup-only", false, "stand each selected test's environment up and skip its body, leaving it recorded and running (forces keep); folds the deprecated `ampqa setup`")
	cmd.Flags().StringVar(&envName, "env-name", "", "label for an env recorded by --env-mode keep / --setup-only (default: the provider's own)")
	cmd.Flags().DurationVar(&envTTL, "ttl", 0, "auto-reap a kept/setup-only env after this long (default 0: never — reap explicitly with env rm)")
	cmd.Flags().StringVar(&regPath, "registry", "", "registry file path for --env / kept envs (default ~/.ampqa/envs.json)")
	cmd.Flags().StringVar(&outDir, "out-dir", "", "write per-result JSON files under <out-dir>/<runid> (e.g. ./ampqa-out); default from report.out_dir")
	cmd.Flags().StringVar(&junitPath, "junit", "", "write a JUnit XML report to this path; default from report.junit_path")
	cmd.Flags().StringVar(&portalURL, "portal", "", "amp report portal base URL — stream the run to it live as it executes; default from report.portal.url")
	cmd.Flags().StringVar(&reportName, "report", "", "portal report bucket to file the run under; default from report.portal.report")
	cmd.Flags().StringVar(&htmlPath, "html", "", "write a self-contained HTML report to this path when the run finishes; default from report.html_path")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer (debugging only)")
	return cmd
}

func excludeSuite(tests []core.Test, suite string) []core.Test {
	out := tests[:0:0]
	for _, t := range tests {
		if t.Suite != suite {
			out = append(out, t)
		}
	}
	return out
}

// excludeSetupOnly drops tests with no Run body — setup-only tests (the presets)
// belong to `ampqa setup`, not `run`.
func excludeSetupOnly(tests []core.Test) []core.Test {
	out := tests[:0:0]
	for _, t := range tests {
		if t.Run != nil {
			out = append(out, t)
		}
	}
	return out
}

func hasFailures(s core.RunSummary) bool {
	return s.Counts[core.StatusFailed]+s.Counts[core.StatusBroken]+
		s.Counts[core.StatusEnvFailed]+s.Counts[core.StatusTimedOut] > 0
}

// Run exit codes beyond cobra's usage (2) / no-match (3):
//
//	0 — every selected test passed (or a mix of passed + skipped)
//	1 — at least one test failed/broke/env-failed/timed-out
//	4 — tests were selected but NONE executed: all skipped (e.g. the provider
//	    was unavailable so every real-env test skipped cleanly). Distinct from a
//	    genuine all-pass so an agent checking only the exit code can't read
//	    "all green" when nothing actually ran.
const exitAllSkipped = 4

// runExitCode maps a run summary to its process exit code. Failures win; then an
// all-skipped run (nothing executed) gets its own code; otherwise 0.
func runExitCode(s core.RunSummary) int {
	if hasFailures(s) {
		return 1
	}
	if s.Total > 0 && s.Counts[core.StatusSkipped] == s.Total {
		return exitAllSkipped
	}
	return 0
}

// buildReporter fans results out to the given sinks. resultSinks (jsonfile,
// junit) are marked CRITICAL so a test's pass/fail record is never dropped under
// backpressure; progress (stdout) is non-critical and may drop cosmetic live
// events, which the caller surfaces via Reporter.Dropped().
// summaryHasFailures reports whether any test ended in a non-green state
// (anything but passed/skipped) — the keep-failed teardown decision.
func summaryHasFailures(sum core.RunSummary) bool {
	for st, n := range sum.Counts {
		if n > 0 && st != core.StatusPassed && st != core.StatusSkipped {
			return true
		}
	}
	return false
}

func buildReporter(queueSize int, progress core.Sink, resultSinks ...core.Sink) *report.Reporter {
	return buildReporterWithLive(queueSize, progress, nil, resultSinks...)
}

// buildReporterWithLive also attaches non-critical live sinks (portal): like
// the stdout progress sink they may drop under backpressure but never block a
// test, while result sinks (jsonfile, junit, html) stay critical.
func buildReporterWithLive(queueSize int, progress core.Sink, live []core.Sink, resultSinks ...core.Sink) *report.Reporter {
	opts := make([]report.SinkOption, 0, 1+len(live)+len(resultSinks))
	if progress != nil {
		opts = append(opts, report.SinkOption{Sink: progress})
	}
	for _, s := range live {
		opts = append(opts, report.SinkOption{Sink: s})
	}
	for _, s := range resultSinks {
		opts = append(opts, report.SinkOption{Sink: s, Critical: true})
	}
	return report.NewReporterWithOptions(opts, queueSize)
}

func runData(seed int64, parallel int, summary core.RunSummary, results []*core.Result, reporterDropped int64) map[string]any {
	counts := map[string]int{}
	for st, n := range summary.Counts {
		counts[string(st)] = n
	}
	stepsDropped, metricsDropped := 0, 0
	rows := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		stepsDropped += r.StepsDropped
		metricsDropped += r.MetricsDropped
		rows = append(rows, map[string]any{
			"name":          r.TestName,
			"suite":         r.Suite,
			"category":      r.Category.String(),
			"status":        string(r.Status),
			"duration_ms":   r.Duration.Milliseconds(),
			"seed":          r.Seed,
			"error_summary": r.ErrorSummary,
			"skip_reason":   r.SkipReason,
		})
	}
	return map[string]any{
		"seed":             seed,
		"parallel":         parallel,
		"total":            summary.Total,
		"counts":           counts,
		"results":          rows,
		"reporter_dropped": reporterDropped,
		"steps_dropped":    stepsDropped,
		"metrics_dropped":  metricsDropped,
	}
}
