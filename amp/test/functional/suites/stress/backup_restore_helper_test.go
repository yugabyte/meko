package stress

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func brCfg(overrides map[string]any) (*core.Config, backupRestoreConfig) {
	base := map[string]any{
		"stress.backing_backup_restore.enabled": true,
		"secrets.gh_package.token":              "not-a-real-token",
	}
	for k, v := range overrides {
		base[k] = v
	}
	c := core.NewConfig()
	for k, v := range base {
		c.Set(k, v)
	}
	return c, backupRestoreFromConfig(c)
}

func TestBackupRestoreGateAcceptsAConfiguredRun(t *testing.T) {
	c, cfg := brCfg(nil)
	if reason, ok := backupRestoreGate(c, cfg); !ok {
		t.Fatalf("gate rejected a complete config: %s", reason)
	}
}

func TestBackupRestoreGateSkipsUnlessEnabled(t *testing.T) {
	c, cfg := brCfg(map[string]any{"stress.backing_backup_restore.enabled": false})
	reason, ok := backupRestoreGate(c, cfg)
	if ok {
		t.Fatal("enabled=false must stop the scenario: it is the only way to keep it off a run")
	}
	if !strings.Contains(reason, "enabled") {
		t.Errorf("skip reason = %q, want it to name the gate key", reason)
	}
}

func TestBackupRestoreGateRejectsRestoreBeforeBackup(t *testing.T) {
	c, cfg := brCfg(map[string]any{
		"stress.backing_backup_restore.operations": []string{"restore", "backup"},
	})
	reason, ok := backupRestoreGate(c, cfg)
	if ok {
		t.Fatal("restore before backup has nothing to restore; the gate must catch it")
	}
	if !strings.Contains(reason, "restore") {
		t.Errorf("skip reason = %q", reason)
	}
}

func TestBackupRestoreGateRejectsAnUnknownOperation(t *testing.T) {
	c, cfg := brCfg(map[string]any{
		"stress.backing_backup_restore.operations": []string{"backup", "explode"},
	})
	if _, ok := backupRestoreGate(c, cfg); ok {
		t.Fatal("unknown operation accepted")
	}
}

func TestBackupRestoreGateAllowsPitrOnly(t *testing.T) {
	c, cfg := brCfg(map[string]any{
		"stress.backing_backup_restore.operations": []string{"pitr"},
	})
	if reason, ok := backupRestoreGate(c, cfg); !ok {
		t.Fatalf("pitr-only rejected: %s", reason)
	}
	if cfg.wantsBackupOps() {
		t.Error("pitr-only run must not demand YBC")
	}
}

func TestBackupRestoreConfigDefaults(t *testing.T) {
	_, cfg := brCfg(nil)
	if cfg.Tenants != DefaultBackupRestoreTenants {
		t.Errorf("tenants = %d, want %d", cfg.Tenants, DefaultBackupRestoreTenants)
	}
	if strings.Join(cfg.Operations, ",") != "backup,restore,pitr,scheduled_backup" {
		t.Errorf("operations = %v", cfg.Operations)
	}
	if !cfg.BackupAmpKeyspace {
		t.Error("backup_amp_keyspace default = false, want true")
	}
	if cfg.ScheduleFrequency != time.Hour || cfg.ScheduleFireTimeout != 10*time.Minute {
		t.Errorf("schedule defaults = %s / %s", cfg.ScheduleFrequency, cfg.ScheduleFireTimeout)
	}
	if !cfg.wantsBackupOps() {
		t.Error("default operations must demand YBC")
	}
}

func TestBackupRestoreConfigReadsItsOwnPrefix(t *testing.T) {
	_, cfg := brCfg(map[string]any{
		"stress.backing_backup_restore.tenants":    5,
		"stress.backing_maintenance.tenants":       9,
		"stress.backing_backup_restore.probe_rows": 42,
	})
	if cfg.Tenants != 5 {
		t.Errorf("tenants = %d, want 5 (must not read the maintenance prefix)", cfg.Tenants)
	}
	if cfg.ProbeRows != 42 {
		t.Errorf("probe_rows = %d, want 42", cfg.ProbeRows)
	}
}
