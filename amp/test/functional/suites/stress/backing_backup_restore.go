// Runs YBA's data-protection operations — full backup, restore, PITR, a
// recurring backup schedule — on the YBA universe backing amp while tenants
// write through their endpoints, asserting the operations succeed AND amp kept
// acknowledging commits through every one of them. Restores never touch amp's
// keyspace: they roll probe databases created for the purpose, while a backup
// of amp's own storage keyspace exercises the distributed snapshot under live
// write traffic. backing_maintenance covers the node-restarting operations.
package stress

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_backup_restore",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Back up, restore, PITR-rewind and schedule backups on the YugabyteDB universe under amp while amp tenants run bank-transfer and SQL-transaction workloads through their endpoints, then assert every operation succeeded, restores round-tripped exactly, and amp acknowledged commits throughout.",
		Params:      []string{"stress.backing_backup_restore", "stress.consistency"},
		Timeout:     4 * time.Hour,
		Run:         runBackingBackupRestore,
	})
}

func runBackingBackupRestore(t *core.T) error {
	cfg := backupRestoreFromConfig(t.Options)
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane: the workload writes THROUGH amp")
		return nil
	}
	target, reason, ok := resolveMaintenanceTarget(t, cfg.maintenanceConfig)
	if !ok {
		t.Skip(reason)
		return nil
	}
	yt, ok := target.(*ybaTarget)
	if !ok || yt.kube == nil {
		t.Skip("the backing is not a kubernetes YBA universe: probe databases and the MinIO backup store need in-cluster access")
		return nil
	}
	backuper, ok := yt.client.(ybaBackuper)
	if !ok {
		t.Skip("this env's YBA client does not expose the backup API")
		return nil
	}
	if reason, ok := backupRestoreGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}

	r := &backupRestoreRun{
		maintenanceRun: &maintenanceRun{
			t:      t,
			cfg:    cfg.maintenanceConfig,
			env:    env,
			target: target,
			logf:   t.Masker().Logf(log.Printf),
		},
		bcfg:       cfg,
		backuper:   backuper,
		universeID: yt.universeID,
		ukube:      yt.kube,
		probeDB:    "ampqa_backup_probe",
		pitrDB:     "ampqa_pitr_probe",
	}
	r.sink = attachSink{t: t, logf: r.logf}

	if u, err := yt.client.GetUniverse(t.Context, yt.universeID); err != nil {
		return fmt.Errorf("read universe %s: %w", yt.universeID, err)
	} else if !u.HasYbc() && cfg.wantsBackupOps() {
		t.Skip("the universe carries no YBC, which YBA's backup tasks require: create it with apps.yba.k8s.enable_ybc=true (apps.yba.universe.enable_ybc for a VM universe)")
		return nil
	}

	fleet, err := newTenantFleet(t, env, r.logf)
	if err != nil {
		return err
	}
	defer fleet.Close()
	r.fleet = fleet
	defer func() {
		if r.loads != nil {
			r.loads.CollectAll(r.sink)
		}
		if r.pod != nil {
			pctx, cancel := context.WithTimeout(context.Background(), reportKillTimeout)
			if err := r.pod.Stop(pctx, r.sink); err != nil {
				r.logf("stop client pod (best-effort): %v", err)
			}
			cancel()
		}
		if r.minio != nil {
			mctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			r.releaseBackupTarget(mctx)
			r.minio.Delete(mctx)
			cancel()
		}
	}()

	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"create_tenants", r.createTenants},
		{"amp_is_backed_by_this_universe", r.assertAmpBackedByTarget},
		{"prepare_backup_target", r.prepareBackupTarget},
		{"baseline_write", r.baselineWrite},
		{"start_endpoint_workloads", r.startEndpointWorkloads},
		{"backup_restore_under_load", r.backupRestoreUnderLoad},
		{"endpoints_serve_again", r.endpointsServeAgain},
		{"writes_flow_again", r.writesFlowAgain},
		{"verify_data", r.verifyData},
		{"physical_consistency", r.physicalConsistency},
	}
	for _, p := range phases {
		if err := t.Step(p.name, func(*core.StepHandle) error { return p.fn(t.Context) }); err != nil {
			return err
		}
	}
	return nil
}

func (cfg backupRestoreConfig) wantsBackupOps() bool {
	for _, op := range cfg.Operations {
		if op == OpBackup || op == OpRestore || op == OpScheduledBackup {
			return true
		}
	}
	return false
}

func (r *backupRestoreRun) backupRestoreUnderLoad(ctx context.Context) error {
	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	done := startTenantWriters(loadCtx, r.env, r.fleet.Tenants, r.cfg.PayloadBytes)

	plan := &maintenancePlan{
		ops:         r.buildBackupOps(),
		progress:    func() int64 { return totalWritten(r.fleet.Tenants) },
		minProgress: int64(r.cfg.MinCommitsPerOp),
		observe: func(op string, commits int64) {
			r.t.Metric("commits_during_"+op, float64(commits), "count")
		},
		after: func(ctx context.Context, op string) error {
			if r.loads == nil {
				return nil
			}
			return r.loads.Check(ctx, op)
		},
		step: func(name string, fn func() error) error {
			return r.t.Step(name, func(*core.StepHandle) error { return fn() })
		},
		logf: r.logf,
	}
	before := totalWritten(r.fleet.Tenants)
	err := plan.run(ctx)

	stopLoad()
	<-done
	r.t.Metric("commits_during_backup_restore", float64(totalWritten(r.fleet.Tenants)-before), "count")
	return err
}

func (r *backupRestoreRun) physicalConsistency(ctx context.Context) error {
	return r.t.Step("amcheck", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(r.t, s, r.env, r.fleet.Tenants)
	})
}
