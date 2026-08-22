// Configuration, gate and operations for stress/backing_backup_restore.
package stress

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/minio"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	OpBackup          = "backup"
	OpRestore         = "restore"
	OpPitr            = "pitr"
	OpScheduledBackup = "scheduled_backup"
)

// Defaults (framework/core/defaults_test.go asserts default.yaml agrees).
const (
	DefaultBackupRestoreTenants        = 2
	DefaultBackupRestoreProbeRows      = 2000
	DefaultBackupRestorePitrRetention  = 4 * time.Hour
	DefaultBackupRestorePitrInterval   = 1 * time.Hour
	DefaultBackupRestorePitrMarkMargin = 5 * time.Second
	DefaultBackupRestoreScheduleFreq   = 1 * time.Hour
	DefaultBackupRestoreScheduleFire   = 10 * time.Minute
	// schedulePoll is well under YBA's 2-minute scheduler tick, so the
	// measured first-fire time is the schedule's latency, not the poll's.
	schedulePoll                      = 15 * time.Second
	DefaultBackupRestoreStorageReady  = 5 * time.Minute
	DefaultBackupRestoreStorageConfig = "ampqa-minio"
)

var DefaultBackupRestoreOperations = []string{OpBackup, OpRestore, OpPitr, OpScheduledBackup}

type backupRestoreConfig struct {
	maintenanceConfig
	BackupAmpKeyspace bool
	StorageConfigName string
	MinioServerImage  string
	MinioClientImage  string
	ProbeRows         int
	PitrRetention     time.Duration
	PitrInterval      time.Duration
	PitrMarkMargin    time.Duration
	ScheduleFrequency time.Duration
	// ScheduleFireTimeout 0 means create-and-verify only; otherwise it bounds
	// the wait for the first scheduled backup, which fires immediately.
	ScheduleFireTimeout time.Duration
	StorageReadyTimeout time.Duration
}

func backupRestoreFromConfig(c *core.Config) backupRestoreConfig {
	const p = "stress.backing_backup_restore."
	cfg := backupRestoreConfig{
		maintenanceConfig:   workloadConfigAt(c, p),
		BackupAmpKeyspace:   c.GetBool(p+"backup_amp_keyspace", true),
		StorageConfigName:   c.GetString(p+"storage_config_name", DefaultBackupRestoreStorageConfig),
		MinioServerImage:    c.GetString(p+"minio_server_image", minio.DefaultServerImage),
		MinioClientImage:    c.GetString(p+"minio_client_image", minio.DefaultClientImage),
		ProbeRows:           c.GetInt(p+"probe_rows", DefaultBackupRestoreProbeRows),
		PitrRetention:       c.GetDuration(p+"pitr_retention", DefaultBackupRestorePitrRetention),
		PitrInterval:        c.GetDuration(p+"pitr_interval", DefaultBackupRestorePitrInterval),
		PitrMarkMargin:      c.GetDuration(p+"pitr_mark_margin", DefaultBackupRestorePitrMarkMargin),
		ScheduleFrequency:   c.GetDuration(p+"schedule_frequency", DefaultBackupRestoreScheduleFreq),
		ScheduleFireTimeout: c.GetDuration(p+"schedule_fire_timeout", DefaultBackupRestoreScheduleFire),
		StorageReadyTimeout: c.GetDuration(p+"storage_ready_timeout", DefaultBackupRestoreStorageReady),
	}
	cfg.Operations = c.GetStringSlice(p+"operations", DefaultBackupRestoreOperations)
	cfg.Tenants = c.GetInt(p+"tenants", DefaultBackupRestoreTenants)
	return cfg
}

// Polices the test's own configuration; the backing comes from the env.
func backupRestoreGate(c *core.Config, cfg backupRestoreConfig) (string, bool) {
	if !cfg.Enabled {
		return "stress.backing_backup_restore.enabled is not set (it drives real backup/restore tasks on the run's backing)", false
	}
	if len(cfg.Operations) == 0 {
		return "stress.backing_backup_restore.operations is empty: there is nothing to run", false
	}
	sawBackup := false
	for _, op := range cfg.Operations {
		switch op {
		case OpBackup:
			sawBackup = true
		case OpRestore:
			if !sawBackup {
				return "operations lists restore before backup: there would be nothing to restore", false
			}
		case OpPitr, OpScheduledBackup:
		default:
			return fmt.Sprintf("unknown operation %q in stress.backing_backup_restore.operations", op), false
		}
	}
	if cfg.Tenants < 1 {
		return "stress.backing_backup_restore.tenants must be at least 1: the amp load IS half the test", false
	}
	secrets, err := core.NewSecrets(c)
	if err != nil {
		return "secrets not configured: " + err.Error(), false
	}
	if _, err := secrets.Value(cfg.JarSecret); err != nil {
		return fmt.Sprintf("sample-app jar secret %q not resolvable: declare it under secrets: (e.g. gh_package: {env: GH_PACKAGE_TOKEN})", cfg.JarSecret), false
	}
	return "", true
}

// ybaBackuper is the backup subset of *yba.Client the suite drives. Local so
// the suite depends on the capability, not the client type.
type ybaBackuper interface {
	EnsureStorageConfig(ctx context.Context, backend, configName string, data map[string]any) (string, error)
	CreateBackupAndWait(ctx context.Context, s yba.BackupSpec) (yba.Backup, error)
	RestoreBackupAndWait(ctx context.Context, universeUUID, storageConfigUUID, keyspace, storageLocation string) error
	CreatePitrConfigAndWait(ctx context.Context, universeUUID, keyspace string, retention, interval time.Duration) (string, error)
	RestorePitrAndWait(ctx context.Context, universeUUID, pitrConfigUUID string, restoreTimeMillis int64) error
	DeletePitrConfig(ctx context.Context, universeUUID, pitrConfigUUID string) error
	CreateBackupScheduleAndWait(ctx context.Context, s yba.BackupSpec) (string, error)
	ListBackupSchedules(ctx context.Context) ([]yba.BackupSchedule, error)
	DeleteBackupSchedule(ctx context.Context, scheduleUUID string) error
	ListUniverseBackups(ctx context.Context, universeUUID string) ([]yba.Backup, error)
	DeleteStorageConfig(ctx context.Context, configUUID string, deleteBackups bool) error
}

type backupRestoreRun struct {
	*maintenanceRun
	bcfg     backupRestoreConfig
	backuper ybaBackuper
	// universeID is the backup/restore target; kube reaches its tserver pods
	// for probe SQL and hosts the MinIO backup store.
	universeID string
	ukube      *k8s.Kubectl

	minio             *minio.Server
	storageConfigUUID string

	probeDB, pitrDB string
	probeRows       int64
	probeSum        int64
	backupLocation  string
	pitrConfigUUID  string
	scheduleUUID    string
}

// universeSQL runs one statement via ysqlsh on the first tserver pod,
// returning trimmed stdout. db "" targets the default yugabyte database.
func (r *backupRestoreRun) universeSQL(ctx context.Context, db, q string) (string, error) {
	if db == "" {
		db = yba.DefaultYSQLDatabase
	}
	pod, err := firstTserverPod(ctx, r.ukube)
	if err != nil {
		return "", err
	}
	cmd := "ysqlsh -h $(hostname)." + tserverHeadlessOf(pod) + " -U yugabyte -d " + db +
		" -v ON_ERROR_STOP=1 -tAc " + shellQuote(q)
	out, err := r.ukube.ForPod(pod, tserverContainer).Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("ysqlsh on %s: %w", pod, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("ysqlsh (db=%s) exited %d: %s", db, out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

func (r *backupRestoreRun) universeSQLInt(ctx context.Context, db, q string) (int64, error) {
	out, err := r.universeSQL(ctx, db, q)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number from %q, got %q: %w", q, out, err)
	}
	return n, nil
}

// universeTimeMillis reads the cluster's own clock, so PITR restore points
// don't inherit coordinator clock skew.
func (r *backupRestoreRun) universeTimeMillis(ctx context.Context) (int64, error) {
	return r.universeSQLInt(ctx, "", "select (extract(epoch from now())*1000)::bigint")
}

const probeTable = "marks"

// seedProbe (re)creates db with rows ids [1..n], v = id*7.
func (r *backupRestoreRun) seedProbe(ctx context.Context, db string, n int) error {
	if _, err := r.universeSQL(ctx, "", "drop database if exists "+db); err != nil {
		return err
	}
	if _, err := r.universeSQL(ctx, "", "create database "+db); err != nil {
		return err
	}
	if _, err := r.universeSQL(ctx, db,
		fmt.Sprintf("create table %s (id int primary key, v bigint)", probeTable)); err != nil {
		return err
	}
	return r.insertProbeRows(ctx, db, 1, n)
}

func (r *backupRestoreRun) insertProbeRows(ctx context.Context, db string, from, to int) error {
	_, err := r.universeSQL(ctx, db,
		fmt.Sprintf("insert into %s select g, g*7 from generate_series(%d,%d) g", probeTable, from, to))
	return err
}

func (r *backupRestoreRun) probeChecksum(ctx context.Context, db string) (count, sum int64, err error) {
	out, err := r.universeSQL(ctx, db,
		fmt.Sprintf("select count(*) || ' ' || coalesce(sum(v),0) from %s", probeTable))
	if err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscanf(out, "%d %d", &count, &sum); err != nil {
		return 0, 0, fmt.Errorf("parse checksum %q: %w", out, err)
	}
	return count, sum, nil
}

// prepareBackupTarget deploys MinIO beside the universe and registers it as a
// YBA storage config. The MinIO name carries the run's resource suffix, so a
// re-run against a standing YBA reaches a NEW in-cluster address: the storage
// config is rewritten rather than adopted, or every backup would target the
// pod the previous run deleted.
func (r *backupRestoreRun) prepareBackupTarget(ctx context.Context) error {
	needsStorage := false
	for _, op := range r.bcfg.Operations {
		if op == OpBackup || op == OpScheduledBackup {
			needsStorage = true
		}
	}
	if !needsStorage {
		return nil
	}
	bucket := "ampqa-backups"
	srv, err := minio.Deploy(ctx, r.ukube, minio.Spec{
		Name:         "ampqa-minio-" + r.t.ResourceSuffix(),
		Bucket:       bucket,
		ServerImage:  r.bcfg.MinioServerImage,
		ClientImage:  r.bcfg.MinioClientImage,
		ReadyTimeout: r.bcfg.StorageReadyTimeout,
	})
	if err != nil {
		return err
	}
	r.minio = srv
	r.logf("minio serving at %s", srv.Endpoint())

	uuid, err := r.backuper.EnsureStorageConfig(ctx, "S3", r.bcfg.StorageConfigName, map[string]any{
		"BACKUP_LOCATION":       "s3://" + bucket,
		"AWS_ACCESS_KEY_ID":     minio.AccessKey,
		"AWS_SECRET_ACCESS_KEY": minio.SecretKey,
		"AWS_HOST_BASE":         srv.Endpoint(),
		"PATH_STYLE_ACCESS":     true,
	})
	if err != nil {
		return fmt.Errorf("register minio as a YBA storage config: %w", err)
	}
	r.storageConfigUUID = uuid
	r.logf("storage config %s -> %s", r.bcfg.StorageConfigName, uuid)
	return nil
}

// releaseBackupTarget drops the storage config with its backups, so a standing
// YBA is not left holding a config addressed to the MinIO teardown removes.
// Best-effort: YBA refuses the delete while a backup is still running.
func (r *backupRestoreRun) releaseBackupTarget(ctx context.Context) {
	if r.storageConfigUUID == "" {
		return
	}
	if err := r.backuper.DeleteStorageConfig(ctx, r.storageConfigUUID, true); err != nil {
		r.logf("delete storage config %s (best-effort): %v", r.storageConfigUUID, err)
	}
}

func (r *backupRestoreRun) buildBackupOps() []maintenanceOp {
	ops := make([]maintenanceOp, 0, len(r.bcfg.Operations))
	for _, name := range r.bcfg.Operations {
		switch name {
		case OpBackup:
			ops = append(ops, maintenanceOp{name, r.opBackup})
		case OpRestore:
			ops = append(ops, maintenanceOp{name, r.opRestore})
		case OpPitr:
			ops = append(ops, maintenanceOp{name, r.opPitr})
		case OpScheduledBackup:
			ops = append(ops, maintenanceOp{name, r.opScheduledBackup})
		}
	}
	return ops
}

// opBackup seeds a probe database and takes a full backup of it — plus one of
// amp's own storage keyspace, whose distributed snapshot under live amp write
// traffic is the interesting case.
func (r *backupRestoreRun) opBackup(ctx context.Context) error {
	if err := r.seedProbe(ctx, r.probeDB, r.bcfg.ProbeRows); err != nil {
		return err
	}
	count, sum, err := r.probeChecksum(ctx, r.probeDB)
	if err != nil {
		return err
	}
	r.probeRows, r.probeSum = count, sum
	r.t.Metric("probe_rows", float64(count), "count")

	start := time.Now()
	b, err := r.backuper.CreateBackupAndWait(ctx, yba.BackupSpec{
		UniverseUUID:      r.universeID,
		StorageConfigUUID: r.storageConfigUUID,
		Keyspace:          r.probeDB,
	})
	if err != nil {
		return err
	}
	r.t.Metric("probe_backup_seconds", time.Since(start).Seconds(), "s")
	r.backupLocation = b.Location(r.probeDB)
	if r.backupLocation == "" {
		return fmt.Errorf("backup %s completed but reports no storage location", b.BackupUUID)
	}
	r.logf("probe backup completed at %s", r.backupLocation)

	if !r.bcfg.BackupAmpKeyspace {
		return nil
	}
	start = time.Now()
	if _, err := r.backuper.CreateBackupAndWait(ctx, yba.BackupSpec{
		UniverseUUID:      r.universeID,
		StorageConfigUUID: r.storageConfigUUID,
		Keyspace:          ampStorageDB,
	}); err != nil {
		return fmt.Errorf("backup of amp's %s keyspace under load: %w", ampStorageDB, err)
	}
	r.t.Metric("amp_keyspace_backup_seconds", time.Since(start).Seconds(), "s")
	return nil
}

// opRestore drops the probe database and restores it from the backup, then
// checks the rows came back exactly.
func (r *backupRestoreRun) opRestore(ctx context.Context) error {
	if r.backupLocation == "" {
		return fmt.Errorf("no backup location recorded: did the backup operation run?")
	}
	if _, err := r.universeSQL(ctx, "", "drop database if exists "+r.probeDB); err != nil {
		return err
	}
	start := time.Now()
	if err := r.backuper.RestoreBackupAndWait(ctx, r.universeID, r.storageConfigUUID, r.probeDB, r.backupLocation); err != nil {
		return err
	}
	r.t.Metric("probe_restore_seconds", time.Since(start).Seconds(), "s")
	count, sum, err := r.probeChecksum(ctx, r.probeDB)
	if err != nil {
		return fmt.Errorf("read the restored %s back: %w", r.probeDB, err)
	}
	if count != r.probeRows || sum != r.probeSum {
		return fmt.Errorf("restored %s holds %d rows (sum %d), backed up %d rows (sum %d): the restore lost or changed data",
			r.probeDB, count, sum, r.probeRows, r.probeSum)
	}
	r.logf("restore verified: %d rows, sum %d", count, sum)
	return nil
}

// opPitr writes marker rows around a captured cluster timestamp and restores
// to it: rows before the mark must survive, rows after it must be gone.
func (r *backupRestoreRun) opPitr(ctx context.Context) error {
	n := r.bcfg.ProbeRows
	if err := r.seedProbe(ctx, r.pitrDB, n); err != nil {
		return err
	}
	pitrUUID, err := r.backuper.CreatePitrConfigAndWait(ctx, r.universeID, r.pitrDB, r.bcfg.PitrRetention, r.bcfg.PitrInterval)
	if err != nil {
		return err
	}
	r.pitrConfigUUID = pitrUUID
	defer func() {
		if err := r.backuper.DeletePitrConfig(context.WithoutCancel(ctx), r.universeID, pitrUUID); err != nil {
			r.logf("delete pitr config (best-effort): %v", err)
		}
	}()

	// The margin keeps the mark strictly between the two writes even if the
	// commit times land within the same clock tick.
	time.Sleep(r.bcfg.PitrMarkMargin)
	mark, err := r.universeTimeMillis(ctx)
	if err != nil {
		return err
	}
	time.Sleep(r.bcfg.PitrMarkMargin)
	if err := r.insertProbeRows(ctx, r.pitrDB, n+1, 2*n); err != nil {
		return err
	}
	count, _, err := r.probeChecksum(ctx, r.pitrDB)
	if err != nil {
		return err
	}
	if count != int64(2*n) {
		return fmt.Errorf("pre-restore %s holds %d rows, wrote %d", r.pitrDB, count, 2*n)
	}

	start := time.Now()
	if err := r.backuper.RestorePitrAndWait(ctx, r.universeID, pitrUUID, mark); err != nil {
		return err
	}
	r.t.Metric("pitr_restore_seconds", time.Since(start).Seconds(), "s")

	count, _, err = r.probeChecksum(ctx, r.pitrDB)
	if err != nil {
		return fmt.Errorf("read %s after the pitr restore: %w", r.pitrDB, err)
	}
	maxID, err := r.universeSQLInt(ctx, r.pitrDB, "select coalesce(max(id),0) from "+probeTable)
	if err != nil {
		return err
	}
	if count != int64(n) || maxID != int64(n) {
		return fmt.Errorf("after restoring %s to the mark: %d rows with max id %d, want exactly the %d pre-mark rows — the restore rolled back to the wrong point",
			r.pitrDB, count, maxID, n)
	}
	r.logf("pitr verified: %d pre-mark rows back, %d post-mark rows gone", n, n)
	return nil
}

// opScheduledBackup creates a recurring backup schedule and (fire timeout
// permitting) waits for its first backup to complete on its own.
func (r *backupRestoreRun) opScheduledBackup(ctx context.Context) error {
	name := "ampqa-sched-" + r.t.ResourceSuffix()
	scheduleUUID, err := r.backuper.CreateBackupScheduleAndWait(ctx, yba.BackupSpec{
		UniverseUUID:      r.universeID,
		StorageConfigUUID: r.storageConfigUUID,
		Keyspace:          r.probeDB,
		ScheduleName:      name,
		Frequency:         r.bcfg.ScheduleFrequency,
	})
	if err != nil {
		return err
	}
	r.scheduleUUID = scheduleUUID
	defer func() {
		if err := r.backuper.DeleteBackupSchedule(context.WithoutCancel(ctx), scheduleUUID); err != nil {
			r.logf("delete backup schedule (best-effort): %v", err)
		}
	}()

	r.logf("schedule %s registered as %s", name, scheduleUUID)

	if r.bcfg.ScheduleFireTimeout <= 0 {
		r.logf("schedule_fire_timeout is 0: not waiting for the first scheduled backup")
		return nil
	}
	start := time.Now()
	if err := r.waitScheduledBackup(ctx, scheduleUUID); err != nil {
		return err
	}
	r.t.Metric("schedule_first_fire_seconds", time.Since(start).Seconds(), "s")
	return nil
}

// waitScheduledBackup polls the universe's backups for one owned by the
// schedule reaching Completed. A Failed scheduled backup fails immediately.
func (r *backupRestoreRun) waitScheduledBackup(ctx context.Context, scheduleUUID string) error {
	deadline := time.Now().Add(r.bcfg.ScheduleFireTimeout)
	for {
		backups, err := r.backuper.ListUniverseBackups(ctx, r.universeID)
		if err != nil {
			return fmt.Errorf("list backups while waiting for the schedule to fire: %w", err)
		}
		for _, b := range backups {
			if b.ScheduleUUID != scheduleUUID {
				continue
			}
			switch b.CommonBackupInfo.State {
			case yba.BackupStateCompleted:
				r.logf("scheduled backup %s completed", b.BackupUUID)
				return nil
			case yba.BackupStateFailed:
				return fmt.Errorf("scheduled backup %s ended Failed", b.BackupUUID)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no scheduled backup completed within %s of creating the schedule (frequency %s)",
				r.bcfg.ScheduleFireTimeout, r.bcfg.ScheduleFrequency)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the scheduled backup: %w", ctx.Err())
		case <-time.After(schedulePoll):
		}
	}
}
