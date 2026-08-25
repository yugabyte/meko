// Configuration, gate and orchestrator for stress/backing_maintenance.
package stress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	OpRollingRestart  = "rolling_restart"
	OpGflagChange     = "gflag_change"
	OpDataMove        = "data_move"
	OpSoftwareUpgrade = "software_upgrade"
)

// Defaults (framework/core/defaults_test.go asserts default.yaml agrees).
const (
	DefaultMaintenanceRF               = 3
	DefaultMaintenanceTenants          = 3
	DefaultMaintenancePayloadBytes     = 256
	DefaultMaintenanceBaselineDuration = 20 * time.Second
	DefaultMaintenanceResumeDuration   = 30 * time.Second
	DefaultMaintenanceUpgradeArch      = "x86_64"
	DefaultMaintenanceGflagKey         = "log_min_seconds_to_retain"
	DefaultMaintenanceGflagValue       = "1800"
	DefaultMaintenanceMinCommits       = 1
	DefaultMaintenanceMaxNodesOut      = 1
	DefaultMaintenanceClientImage      = "eclipse-temurin:21-jdk"
	DefaultMaintenanceRemoteDir        = "/opt/ampqa"
	DefaultMaintenanceJarSecret        = "gh_package"
	DefaultMaintenanceThreadsRead      = 1
	DefaultMaintenanceThreadsWrite     = 2
	DefaultMaintenanceWorkloadRunTime  = 4 * time.Hour
	DefaultMaintenanceBenchWarehouses  = 1
	DefaultMaintenanceBenchTerminals   = 2
	DefaultMaintenanceBenchStartGrace  = 30 * time.Second
	DefaultMaintenanceBankAccounts     = 200
	DefaultMaintenanceNodeRestartSleep = 180 * time.Second
	DefaultMaintenanceMoveTimeout      = 30 * time.Minute
	DefaultMaintenanceSettleTimeout    = 10 * time.Minute
)

// rolling_restart, data_move and software_upgrade stay selectable but are off
// by default: the first proves strictly less than gflag_change, the second
// needs more nodes than the default universe's RF gives it, and the third needs
// the universe created a train behind the target, which a sweep on the current
// release is not.
var DefaultMaintenanceOperations = []string{OpGflagChange}

var DefaultMaintenanceWorkloads = []string{"SqlInserts", "SqlTransactions", "SqlBankTransfer"}

type maintenanceConfig struct {
	Enabled               bool
	RF                    int
	Operations            []string
	Tenants               int
	PayloadBytes          int
	BaselineDuration      time.Duration
	ResumeDuration        time.Duration
	UpgradeTo             string
	UpgradeImageTag       string
	UpgradeArch           string
	GflagKey              string
	GflagValue            string
	MinCommitsPerOp       int
	MaxNodesOutAtOnce     int
	ClientImage           string
	ClientPullSecret      string
	RemoteDir             string
	JarSecret             string
	WorkloadThreadsRead   int
	WorkloadThreadsWrite  int
	WorkloadRunTime       time.Duration
	BenchWarehouses       int
	BenchTerminals        int
	BenchStartGrace       time.Duration
	BankAccounts          int
	EndpointCreds         endpointCreds
	RollingSampleInterval time.Duration
	NodeRestartSleep      time.Duration
	MoveTimeout           time.Duration
	SettleTimeout         time.Duration
}

func maintenanceFromConfig(c *core.Config) maintenanceConfig {
	const p = "stress.backing_maintenance."
	cfg := workloadConfigAt(c, p)
	cfg.RF = c.GetInt(p+"rf", DefaultMaintenanceRF)
	cfg.Operations = c.GetStringSlice(p+"operations", DefaultMaintenanceOperations)
	cfg.UpgradeTo = c.GetString(p+"upgrade_to", "")
	cfg.UpgradeImageTag = c.GetString(p+"upgrade_image_tag", "")
	cfg.UpgradeArch = c.GetString(p+"upgrade_arch", DefaultMaintenanceUpgradeArch)
	cfg.GflagKey = c.GetString(p+"gflag_key", DefaultMaintenanceGflagKey)
	cfg.GflagValue = c.GetString(p+"gflag_value", DefaultMaintenanceGflagValue)
	cfg.MaxNodesOutAtOnce = c.GetInt(p+"max_nodes_out_at_once", DefaultMaintenanceMaxNodesOut)
	cfg.RollingSampleInterval = c.GetDuration(p+"rolling_sample_interval", DefaultRollingSampleInterval)
	cfg.NodeRestartSleep = c.GetDuration(p+"node_restart_sleep", DefaultMaintenanceNodeRestartSleep)
	cfg.MoveTimeout = c.GetDuration(p+"move_timeout", DefaultMaintenanceMoveTimeout)
	if cfg.UpgradeImageTag == "" {
		cfg.UpgradeImageTag = cfg.UpgradeTo
	}
	return cfg
}

// workloadConfigAt reads only the knobs shared by every suite that drives amp
// tenant load while operating on the backing: the tenants, their workloads and
// the settle/commit budgets. The node-restarting knobs (rf, gflags, upgrade
// target, rolling limits) stay with maintenanceFromConfig — a prefix that
// silently answered for them would document knobs its own suite never reads.
func workloadConfigAt(c *core.Config, p string) maintenanceConfig {
	return maintenanceConfig{
		Enabled:              c.GetBool(p+"enabled", true),
		Tenants:              c.GetInt(p+"tenants", DefaultMaintenanceTenants),
		PayloadBytes:         c.GetInt(p+"payload_bytes", DefaultMaintenancePayloadBytes),
		BaselineDuration:     c.GetDuration(p+"baseline_duration", DefaultMaintenanceBaselineDuration),
		ResumeDuration:       c.GetDuration(p+"resume_duration", DefaultMaintenanceResumeDuration),
		MinCommitsPerOp:      c.GetInt(p+"min_commits_per_op", DefaultMaintenanceMinCommits),
		ClientImage:          c.GetString(p+"client_image", DefaultMaintenanceClientImage),
		ClientPullSecret:     c.GetString(p+"client_pull_secret", ""),
		RemoteDir:            c.GetString(p+"remote_dir", DefaultMaintenanceRemoteDir),
		JarSecret:            c.GetString(p+"jar_secret", DefaultMaintenanceJarSecret),
		WorkloadThreadsRead:  c.GetInt(p+"workload_threads_read", DefaultMaintenanceThreadsRead),
		WorkloadThreadsWrite: c.GetInt(p+"workload_threads_write", DefaultMaintenanceThreadsWrite),
		WorkloadRunTime:      c.GetDuration(p+"workload_run_time", DefaultMaintenanceWorkloadRunTime),
		BenchWarehouses:      c.GetInt(p+"benchbase_warehouses", DefaultMaintenanceBenchWarehouses),
		BenchTerminals:       c.GetInt(p+"benchbase_terminals", DefaultMaintenanceBenchTerminals),
		BenchStartGrace:      c.GetDuration(p+"benchbase_start_grace", DefaultMaintenanceBenchStartGrace),
		BankAccounts:         c.GetInt(p+"bank_accounts", DefaultMaintenanceBankAccounts),
		// An amp endpoint provisions cloud_admin/postgres, not the jar's yugabyte defaults.
		EndpointCreds: endpointCreds{
			User:     c.GetString("functional.amp.endpoint_user", "cloud_admin"),
			Password: c.GetString("functional.amp.endpoint_password", "cloud_admin"),
			Database: c.GetString("functional.amp.endpoint_db", "postgres"),
		},
		SettleTimeout: c.GetDuration(p+"settle_timeout", DefaultMaintenanceSettleTimeout),
	}
}

func (cfg maintenanceConfig) wants(op string) bool {
	for _, o := range cfg.Operations {
		if o == op {
			return true
		}
	}
	return false
}

// Polices the test's own configuration; the backing and control plane come from the env.
func maintenanceGate(c *core.Config, cfg maintenanceConfig) (string, bool) {
	if !cfg.Enabled {
		return "stress.backing_maintenance.enabled is false: the scenario was turned off for this run", false
	}
	if len(cfg.Operations) == 0 {
		return "stress.backing_maintenance.operations is empty: there is no maintenance to run", false
	}
	for _, op := range cfg.Operations {
		switch op {
		case OpRollingRestart, OpGflagChange, OpDataMove, OpSoftwareUpgrade:
		default:
			return fmt.Sprintf("unknown maintenance operation %q in stress.backing_maintenance.operations", op), false
		}
	}
	// Below RF=3 a node going out stops the writes for a legitimate reason, so
	// the run would measure the wrong thing rather than find a bug.
	if cfg.RF < 3 {
		return fmt.Sprintf("stress.backing_maintenance.rf is %d: maintenance under load needs RF>=3 to stay writable while a node is out", cfg.RF), false
	}
	if cfg.Tenants < 1 {
		return "stress.backing_maintenance.tenants must be at least 1: the workload IS the test", false
	}
	// No jar credential means no workload, which is a skip rather than a red
	// run. The secret VALUE never reaches the reason.
	secrets, err := core.NewSecrets(c)
	if err != nil {
		return "secrets not configured: " + err.Error(), false
	}
	if _, err := secrets.Value(cfg.JarSecret); err != nil {
		return fmt.Sprintf("sample-app jar secret %q not resolvable: declare it under secrets: (e.g. gh_package: {env: GH_PACKAGE_TOKEN})", cfg.JarSecret), false
	}
	return "", true
}

// --- the orchestrator (pure; unit-tested with fakes) ---

type maintenanceOp struct {
	name string
	run  func(context.Context) error
}

// Runs each op in order while the workloads keep writing, asserting after each
// one that the backing kept acknowledging commits THROUGHOUT it.
type maintenancePlan struct {
	ops         []maintenanceOp
	progress    func() int64 // acknowledged commits so far
	minProgress int64        // commits an op must produce to count as writable
	observe     func(op string, commits int64)
	after       func(ctx context.Context, op string) error
	// step reports each operation separately, so a reader can tell which one failed.
	step func(name string, fn func() error) error
	logf func(string, ...any)
}

func (p *maintenancePlan) run(ctx context.Context) (err error) {
	if len(p.ops) == 0 {
		return fmt.Errorf("no maintenance operations to run")
	}

	for _, op := range p.ops {
		before := p.progress()
		run := func() error { return op.run(ctx) }
		if p.step != nil {
			s := p.step
			run = func() error { return s(op.name, func() error { return op.run(ctx) }) }
		}
		runErr := run()

		commits := p.progress() - before
		if p.observe != nil {
			p.observe(op.name, commits)
		}
		if p.logf != nil {
			p.logf("maintenance %s done: %d commits acknowledged during it", op.name, commits)
		}

		if runErr != nil {
			return fmt.Errorf("maintenance %s: %w", op.name, runErr)
		}
		if p.after != nil {
			if e := p.after(ctx, op.name); e != nil {
				return e
			}
		}
		if commits < p.minProgress {
			return fmt.Errorf("the backing acknowledged %d commits during %s (want at least %d): the write path did not survive the maintenance",
				commits, op.name, p.minProgress)
		}
	}

	return nil
}

// Runs until ctx is cancelled, since a maintenance run's length isn't known up front.
func startTenantWriters(ctx context.Context, env ampTenantEnv, tenants []tenant, payloadBytes int) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(1)
		go func(ten *tenant) {
			defer wg.Done()
			writeFor(ctx, env, ten, payloadBytes)
		}(&tenants[i])
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// ampStorageDB holds amp's registry, storage metadata and WALs.
const ampStorageDB = "yb_storage"

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
