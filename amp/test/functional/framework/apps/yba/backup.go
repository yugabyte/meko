// Backup, restore, PITR and scheduled backups on an existing universe, plus
// the storage configs they write to. YSQL only — the framework has no YCQL
// surface. Backup, restore and PITR are async YBA tasks polled via WaitTask;
// the storage-config and schedule-delete calls are plain synchronous requests.
package yba

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// BackupTypePGSQL is YBA's table-type tag for YSQL backups.
const BackupTypePGSQL = "PGSQL_TABLE_TYPE"

// Backup states (commonBackupInfo.state).
const (
	BackupStateCompleted  = "Completed"
	BackupStateInProgress = "InProgress"
	BackupStateFailed     = "Failed"
)

// CustomerConfig is a customer-scoped config row; storage configs have
// Type=STORAGE and Name naming the backend (S3, GCS, NFS, AZ).
type CustomerConfig struct {
	ConfigUUID string `json:"configUUID"`
	ConfigName string `json:"configName"`
	Name       string `json:"name"`
	Type       string `json:"type"`
}

// ListCustomerConfigs returns every customer config (storage, alerts, ...).
func (c *Client) ListCustomerConfigs(ctx context.Context) ([]CustomerConfig, error) {
	var out []CustomerConfig
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/configs"), nil, &out)
	return out, err
}

// CreateStorageConfig creates a storage config. backend is YBA's backend tag
// (S3, GCS, NFS, AZ); data is the backend-specific field map (BACKUP_LOCATION
// plus credentials). Returns the configUUID.
func (c *Client) CreateStorageConfig(ctx context.Context, backend, configName string, data map[string]any) (string, error) {
	body := map[string]any{
		"type":       "STORAGE",
		"name":       backend,
		"configName": configName,
		"data":       data,
	}
	var out struct {
		ConfigUUID string `json:"configUUID"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/configs"), body, &out); err != nil {
		return "", fmt.Errorf("create storage config %s: %w", configName, err)
	}
	return out.ConfigUUID, nil
}

// EnsureStorageConfig returns the configUUID of the storage config named
// configName, creating it when absent. An existing config has its data
// REWRITTEN: the caller's backup location and credentials are authoritative,
// and adopting a stale row silently points every backup at whatever the
// previous owner configured.
func (c *Client) EnsureStorageConfig(ctx context.Context, backend, configName string, data map[string]any) (string, error) {
	configs, err := c.ListCustomerConfigs(ctx)
	if err != nil {
		return "", fmt.Errorf("list customer configs: %w", err)
	}
	for _, cc := range configs {
		if cc.Type == "STORAGE" && cc.ConfigName == configName {
			if err := c.EditStorageConfig(ctx, cc.ConfigUUID, backend, configName, data); err != nil {
				return "", fmt.Errorf("refresh existing storage config %s: %w", configName, err)
			}
			return cc.ConfigUUID, nil
		}
	}
	return c.CreateStorageConfig(ctx, backend, configName, data)
}

// EditStorageConfig overwrites an existing storage config's data.
func (c *Client) EditStorageConfig(ctx context.Context, configUUID, backend, configName string, data map[string]any) error {
	body := map[string]any{
		"type":       "STORAGE",
		"name":       backend,
		"configName": configName,
		"data":       data,
	}
	_, err := c.doJSON(ctx, http.MethodPut, c.custURL("/configs/"+configUUID+"/edit"), body, nil)
	return err
}

// DeleteStorageConfig removes a storage config. deleteBackups also deletes the
// backups written through it, which YBA otherwise refuses to orphan.
func (c *Client) DeleteStorageConfig(ctx context.Context, configUUID string, deleteBackups bool) error {
	url := fmt.Sprintf("%s?isDeleteBackups=%t", c.custURL("/configs/"+configUUID+"/delete"), deleteBackups)
	_, err := c.doJSON(ctx, http.MethodDelete, url, nil, nil)
	return err
}

// BackupSpec names what to back up and where. Frequency fields are only read
// by CreateBackupSchedule.
type BackupSpec struct {
	UniverseUUID      string
	StorageConfigUUID string
	Keyspace          string
	// ScheduleName + Frequency turn the spec into a schedule.
	ScheduleName string
	Frequency    time.Duration
}

func (c *Client) backupRequestBody(s BackupSpec) map[string]any {
	body := map[string]any{
		"universeUUID":      s.UniverseUUID,
		"customerUUID":      c.customerUUID,
		"storageConfigUUID": s.StorageConfigUUID,
		"backupType":        BackupTypePGSQL,
		"keyspaceTableList": []map[string]any{{"keyspace": s.Keyspace}},
		"timeBeforeDelete":  int64(24 * time.Hour / time.Millisecond),
		"expiryTimeUnit":    "DAYS",
	}
	if s.ScheduleName != "" {
		body["scheduleName"] = s.ScheduleName
		body["schedulingFrequency"] = s.Frequency.Milliseconds()
		body["frequencyTimeUnit"] = "HOURS"
	}
	return body
}

// CreateBackup plans an async full YSQL backup of one keyspace. The returned
// TaskRef carries no ResourceUUID (BackupsController.createBackupYb returns
// only the task); resolve the backup via ListUniverseBackups by taskUUID.
func (c *Client) CreateBackup(ctx context.Context, s BackupSpec) (TaskRef, error) {
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/backups"), c.backupRequestBody(s), &out)
	return out, err
}

// Backup is one backup's state and locations, read back after the task.
type Backup struct {
	BackupUUID       string `json:"backupUUID"`
	ScheduleUUID     string `json:"scheduleUUID"`
	CommonBackupInfo struct {
		State        string `json:"state"`
		TaskUUID     string `json:"taskUUID"`
		ResponseList []struct {
			Keyspace        string `json:"keyspace"`
			StorageLocation string `json:"storageLocation"`
			DefaultLocation string `json:"defaultLocation"`
		} `json:"responseList"`
	} `json:"commonBackupInfo"`
}

// Location returns the backup's storage location for keyspace ("" = first),
// preferring storageLocation over defaultLocation.
func (b Backup) Location(keyspace string) string {
	for _, r := range b.CommonBackupInfo.ResponseList {
		if keyspace != "" && r.Keyspace != keyspace {
			continue
		}
		if r.StorageLocation != "" {
			return r.StorageLocation
		}
		return r.DefaultLocation
	}
	return ""
}

// CreateBackupAndWait backs up one keyspace and returns the completed backup
// with its storage location, resolved from the backup list by task UUID. A
// task that succeeds but leaves the backup in a non-Completed state is an
// error — the location would point at garbage.
func (c *Client) CreateBackupAndWait(ctx context.Context, s BackupSpec) (Backup, error) {
	ref, err := c.CreateBackup(ctx, s)
	if err != nil {
		return Backup{}, fmt.Errorf("create backup of %s: %w", s.Keyspace, err)
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return Backup{}, fmt.Errorf("backup of %s: %w", s.Keyspace, err)
	}
	backups, err := c.ListUniverseBackups(ctx, s.UniverseUUID)
	if err != nil {
		return Backup{}, fmt.Errorf("list backups after backing %s up: %w", s.Keyspace, err)
	}
	for _, b := range backups {
		if b.CommonBackupInfo.TaskUUID != ref.TaskUUID {
			continue
		}
		if b.CommonBackupInfo.State != BackupStateCompleted {
			return b, fmt.Errorf("backup %s of %s ended state %q, want %s", b.BackupUUID, s.Keyspace, b.CommonBackupInfo.State, BackupStateCompleted)
		}
		return b, nil
	}
	return Backup{}, fmt.Errorf("backup task %s of %s succeeded but no backup carries that task", ref.TaskUUID, s.Keyspace)
}

// ListUniverseBackups pages the universe's backups newest-first.
func (c *Client) ListUniverseBackups(ctx context.Context, universeUUID string) ([]Backup, error) {
	body := map[string]any{
		"filter":    map[string]any{"universeUUIDList": []string{universeUUID}},
		"limit":     100,
		"offset":    0,
		"direction": "DESC",
		"sortBy":    "createTime",
	}
	var out struct {
		Entities []Backup `json:"entities"`
	}
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/backups/page"), body, &out)
	return out.Entities, err
}

// RestoreBackup plans an async restore of one backed-up keyspace into
// keyspace (same name = recreate, different = rename-on-restore).
func (c *Client) RestoreBackup(ctx context.Context, universeUUID, storageConfigUUID, keyspace, storageLocation string) (TaskRef, error) {
	body := map[string]any{
		"customerUUID":      c.customerUUID,
		"universeUUID":      universeUUID,
		"storageConfigUUID": storageConfigUUID,
		"actionType":        "RESTORE",
		"backupStorageInfoList": []map[string]any{{
			"backupType":      BackupTypePGSQL,
			"keyspace":        keyspace,
			"storageLocation": storageLocation,
		}},
	}
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/restore"), body, &out)
	return out, err
}

// RestoreBackupAndWait restores one keyspace and waits for the task.
func (c *Client) RestoreBackupAndWait(ctx context.Context, universeUUID, storageConfigUUID, keyspace, storageLocation string) error {
	ref, err := c.RestoreBackup(ctx, universeUUID, storageConfigUUID, keyspace, storageLocation)
	if err != nil {
		return fmt.Errorf("restore %s: %w", keyspace, err)
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return fmt.Errorf("restore %s: %w", keyspace, err)
	}
	return nil
}

// PitrConfig is one PITR (snapshot-schedule) config on a universe keyspace.
type PitrConfig struct {
	UUID              string `json:"uuid"`
	DBName            string `json:"dbName"`
	State             string `json:"state"`
	MinRecoverTimeMs  int64  `json:"minRecoverTimeInMillis"`
	MaxRecoverTimeMs  int64  `json:"maxRecoverTimeInMillis"`
	RetentionPeriodMs int64  `json:"retentionPeriod"`
}

// CreatePitrConfig plans PITR on a YSQL keyspace. The returned TaskRef
// carries no ResourceUUID; resolve the config via ListPitrConfigs by DBName.
// Restores only reach back to the config's creation, never before it.
func (c *Client) CreatePitrConfig(ctx context.Context, universeUUID, keyspace string, retention, interval time.Duration) (TaskRef, error) {
	body := map[string]any{
		"retentionPeriodInSeconds": int64(retention.Seconds()),
		"intervalInSeconds":        int64(interval.Seconds()),
	}
	var out TaskRef
	url := c.custURL("/universes/" + universeUUID + "/keyspaces/YSQL/" + keyspace + "/pitr_config")
	_, err := c.doJSON(ctx, http.MethodPost, url, body, &out)
	return out, err
}

// CreatePitrConfigAndWait creates a PITR config and returns its UUID once the
// task (and with it the first covering snapshot) is done.
func (c *Client) CreatePitrConfigAndWait(ctx context.Context, universeUUID, keyspace string, retention, interval time.Duration) (string, error) {
	ref, err := c.CreatePitrConfig(ctx, universeUUID, keyspace, retention, interval)
	if err != nil {
		return "", fmt.Errorf("create pitr config on %s: %w", keyspace, err)
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return "", fmt.Errorf("pitr config on %s: %w", keyspace, err)
	}
	configs, err := c.ListPitrConfigs(ctx, universeUUID)
	if err != nil {
		return "", fmt.Errorf("list pitr configs after creating one on %s: %w", keyspace, err)
	}
	for _, p := range configs {
		if p.DBName == keyspace {
			return p.UUID, nil
		}
	}
	return "", fmt.Errorf("pitr config task on %s succeeded but no config lists that keyspace", keyspace)
}

// ListPitrConfigs lists the universe's PITR configs.
func (c *Client) ListPitrConfigs(ctx context.Context, universeUUID string) ([]PitrConfig, error) {
	var out []PitrConfig
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeUUID+"/pitr_config"), nil, &out)
	return out, err
}

// RestorePitr plans a restore of the config's keyspace to restoreTimeMillis
// (epoch ms, must fall inside the config's recoverable window).
func (c *Client) RestorePitr(ctx context.Context, universeUUID, pitrConfigUUID string, restoreTimeMillis int64) (TaskRef, error) {
	body := map[string]any{
		"pitrConfigUUID":      pitrConfigUUID,
		"restoreTimeInMillis": restoreTimeMillis,
	}
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/universes/"+universeUUID+"/pitr"), body, &out)
	return out, err
}

// RestorePitrAndWait restores to a point in time and waits for the task.
func (c *Client) RestorePitrAndWait(ctx context.Context, universeUUID, pitrConfigUUID string, restoreTimeMillis int64) error {
	ref, err := c.RestorePitr(ctx, universeUUID, pitrConfigUUID, restoreTimeMillis)
	if err != nil {
		return fmt.Errorf("pitr restore: %w", err)
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return fmt.Errorf("pitr restore: %w", err)
	}
	return nil
}

// DeletePitrConfig removes a PITR config (async task, waited).
func (c *Client) DeletePitrConfig(ctx context.Context, universeUUID, pitrConfigUUID string) error {
	var out TaskRef
	url := c.custURL("/universes/" + universeUUID + "/pitr_config/" + pitrConfigUUID)
	if _, err := c.doJSON(ctx, http.MethodDelete, url, nil, &out); err != nil {
		return fmt.Errorf("delete pitr config %s: %w", pitrConfigUUID, err)
	}
	if out.TaskUUID != "" {
		if _, err := c.WaitTask(ctx, out.TaskUUID); err != nil {
			return fmt.Errorf("delete pitr config %s: %w", pitrConfigUUID, err)
		}
	}
	return nil
}

// BackupSchedule is one scheduled-backup row.
type BackupSchedule struct {
	ScheduleUUID string `json:"scheduleUUID"`
	ScheduleName string `json:"scheduleName"`
	Status       string `json:"status"`
	Frequency    int64  `json:"frequency"`
}

// CreateBackupSchedule plans a recurring full backup of spec's keyspace every
// spec.Frequency (YBA's floor is 1h). The returned TaskRef carries no
// ResourceUUID; resolve the schedule via ListBackupSchedules by name. The
// FIRST backup fires immediately rather than a period later — with a frequency
// and no prior run, Schedule.nextExpectedTaskTime returns now, and the
// scheduler picks it up within its 2-minute tick.
func (c *Client) CreateBackupSchedule(ctx context.Context, s BackupSpec) (TaskRef, error) {
	if s.ScheduleName == "" || s.Frequency <= 0 {
		return TaskRef{}, fmt.Errorf("backup schedule needs a name and a positive frequency")
	}
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/create_backup_schedule_async"), c.backupRequestBody(s), &out)
	return out, err
}

// CreateBackupScheduleAndWait creates a schedule and waits for its task,
// returning the scheduleUUID.
func (c *Client) CreateBackupScheduleAndWait(ctx context.Context, s BackupSpec) (string, error) {
	ref, err := c.CreateBackupSchedule(ctx, s)
	if err != nil {
		return "", fmt.Errorf("create backup schedule %s: %w", s.ScheduleName, err)
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return "", fmt.Errorf("backup schedule %s: %w", s.ScheduleName, err)
	}
	schedules, err := c.ListBackupSchedules(ctx)
	if err != nil {
		return "", fmt.Errorf("list schedules after creating %s: %w", s.ScheduleName, err)
	}
	for _, sch := range schedules {
		if sch.ScheduleName == s.ScheduleName {
			return sch.ScheduleUUID, nil
		}
	}
	return "", fmt.Errorf("schedule task for %s succeeded but no schedule carries that name", s.ScheduleName)
}

// ListBackupSchedules pages the customer's backup schedules.
func (c *Client) ListBackupSchedules(ctx context.Context) ([]BackupSchedule, error) {
	body := map[string]any{
		"filter":    map[string]any{},
		"limit":     100,
		"offset":    0,
		"direction": "DESC",
		"sortBy":    "taskType",
	}
	var out struct {
		Entities []BackupSchedule `json:"entities"`
	}
	_, err := c.doJSON(ctx, http.MethodPost, c.custURL("/schedules/page"), body, &out)
	return out.Entities, err
}

// DeleteBackupSchedule removes a schedule.
func (c *Client) DeleteBackupSchedule(ctx context.Context, scheduleUUID string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, c.custURL("/schedules/"+scheduleUUID), nil, nil)
	return err
}
