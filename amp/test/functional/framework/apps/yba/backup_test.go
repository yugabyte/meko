package yba

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func taskDone(f *fakeYBA, taskUUID string) {
	f.on("GET /api/v1/customers/cust1/tasks/"+taskUUID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": TaskSuccess, "percent": 100.0})
	})
}

// An adopted config must have its data REWRITTEN: a second run stands up a new
// MinIO at a new in-cluster address, and reusing the first run's row would send
// every backup to a pod that no longer exists.
func TestEnsureStorageConfigRewritesAnExistingConfig(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{
			{"configUUID": "sc-1", "configName": "minio-1", "name": "S3", "type": "STORAGE"},
		})
	})
	edited := false
	f.on("PUT /api/v1/customers/cust1/configs/sc-1/edit", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		data, _ := req["data"].(map[string]any)
		if data["AWS_HOST_BASE"] != "http://minio-run2:9000" {
			t.Errorf("edit did not carry the new host: %v", data)
		}
		edited = true
		writeJSON(w, 200, map[string]any{"configUUID": "sc-1"})
	})

	uuid, err := f.client().EnsureStorageConfig(context.Background(), "S3", "minio-1", map[string]any{
		"BACKUP_LOCATION": "s3://b", "AWS_HOST_BASE": "http://minio-run2:9000",
	})
	if err != nil || uuid != "sc-1" {
		t.Fatalf("ensure: %q, %v", uuid, err)
	}
	if !edited {
		t.Error("an existing config was adopted without rewriting its data")
	}
}

func TestDeleteStorageConfigPassesDeleteBackups(t *testing.T) {
	f := newFake()
	defer f.close()
	var query string
	f.on("DELETE /api/v1/customers/cust1/configs/sc-1/delete", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeJSON(w, 200, map[string]any{})
	})
	if err := f.client().DeleteStorageConfig(context.Background(), "sc-1", true); err != nil {
		t.Fatal(err)
	}
	if query != "isDeleteBackups=true" {
		t.Errorf("query = %q, want isDeleteBackups=true", query)
	}
}

func TestDeleteBackupSchedule(t *testing.T) {
	f := newFake()
	defer f.close()
	called := false
	f.on("DELETE /api/v1/customers/cust1/schedules/sched-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, 200, map[string]any{})
	})
	if err := f.client().DeleteBackupSchedule(context.Background(), "sched-1"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("no delete reached the schedule route")
	}
}

func TestEnsureStorageConfigCreatesWhenAbsent(t *testing.T) {
	f := newFake()
	defer f.close()
	existing := []map[string]any{}
	f.on("GET /api/v1/customers/cust1/configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, existing)
	})
	f.on("POST /api/v1/customers/cust1/configs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["type"] != "STORAGE" || req["name"] != "S3" || req["configName"] != "minio-1" {
			t.Errorf("unexpected create body: %v", req)
		}
		data, _ := req["data"].(map[string]any)
		if data["BACKUP_LOCATION"] != "s3://b" {
			t.Errorf("data = %v", data)
		}
		existing = append(existing, map[string]any{"configUUID": "sc-1", "configName": "minio-1", "name": "S3", "type": "STORAGE"})
		writeJSON(w, 200, map[string]any{"configUUID": "sc-1"})
	})
	f.on("PUT /api/v1/customers/cust1/configs/sc-1/edit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"configUUID": "sc-1"})
	})

	c := f.client()
	uuid, err := c.EnsureStorageConfig(context.Background(), "S3", "minio-1", map[string]any{"BACKUP_LOCATION": "s3://b"})
	if err != nil || uuid != "sc-1" {
		t.Fatalf("first ensure: %q, %v", uuid, err)
	}
	uuid, err = c.EnsureStorageConfig(context.Background(), "S3", "minio-1", nil)
	if err != nil || uuid != "sc-1" {
		t.Fatalf("second ensure: %q, %v", uuid, err)
	}
	creates := 0
	for _, req := range f.requests {
		if req == "POST /api/v1/customers/cust1/configs" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("created %d times, want 1", creates)
	}
}

func TestCreateBackupAndWaitReturnsLocation(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/backups", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["backupType"] != BackupTypePGSQL || req["storageConfigUUID"] != "sc-1" {
			t.Errorf("backup body: %v", req)
		}
		if _, hasSched := req["scheduleName"]; hasSched {
			t.Errorf("plain backup must not carry scheduleName: %v", req)
		}
		// The real controller returns only the task; the backup is resolved
		// by listing.
		writeJSON(w, 200, map[string]any{"taskUUID": "t-1"})
	})
	taskDone(f, "t-1")
	f.on("POST /api/v1/customers/cust1/backups/page", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entities": []map[string]any{
			{
				"backupUUID": "b-other",
				"commonBackupInfo": map[string]any{
					"state": BackupStateCompleted, "taskUUID": "t-other",
				},
			},
			{
				"backupUUID": "b-1",
				"commonBackupInfo": map[string]any{
					"state":    BackupStateCompleted,
					"taskUUID": "t-1",
					"responseList": []map[string]any{
						{"keyspace": "probe", "storageLocation": "s3://b/loc1", "defaultLocation": "s3://b/def1"},
					},
				},
			},
		}})
	})

	b, err := f.client().CreateBackupAndWait(context.Background(), BackupSpec{
		UniverseUUID: "u-1", StorageConfigUUID: "sc-1", Keyspace: "probe",
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.BackupUUID != "b-1" {
		t.Errorf("resolved backup %q by task, want b-1", b.BackupUUID)
	}
	if got := b.Location("probe"); got != "s3://b/loc1" {
		t.Errorf("location = %q", got)
	}
}

func TestCreateBackupAndWaitRejectsNonCompletedState(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/backups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"taskUUID": "t-1"})
	})
	taskDone(f, "t-1")
	f.on("POST /api/v1/customers/cust1/backups/page", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entities": []map[string]any{{
			"backupUUID":       "b-1",
			"commonBackupInfo": map[string]any{"state": BackupStateFailed, "taskUUID": "t-1"},
		}}})
	})
	if _, err := f.client().CreateBackupAndWait(context.Background(), BackupSpec{
		UniverseUUID: "u-1", StorageConfigUUID: "sc-1", Keyspace: "probe",
	}); err == nil {
		t.Fatal("want an error for a Failed backup behind a successful task")
	}
}

func TestRestoreBackupAndWait(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/restore", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ActionType string `json:"actionType"`
			List       []struct {
				BackupType      string `json:"backupType"`
				Keyspace        string `json:"keyspace"`
				StorageLocation string `json:"storageLocation"`
			} `json:"backupStorageInfoList"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ActionType != "RESTORE" || len(req.List) != 1 || req.List[0].Keyspace != "probe" || req.List[0].StorageLocation != "s3://b/loc1" {
			t.Errorf("restore body: %s", body)
		}
		writeJSON(w, 200, map[string]any{"taskUUID": "t-9"})
	})
	taskDone(f, "t-9")
	if err := f.client().RestoreBackupAndWait(context.Background(), "u-1", "sc-1", "probe", "s3://b/loc1"); err != nil {
		t.Fatal(err)
	}
}

func TestPitrCreateRestoreDelete(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/universes/u-1/keyspaces/YSQL/pdb/pitr_config", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["retentionPeriodInSeconds"] != float64(14400) || req["intervalInSeconds"] != float64(3600) {
			t.Errorf("pitr body: %v", req)
		}
		writeJSON(w, 200, map[string]any{"taskUUID": "t-p"})
	})
	taskDone(f, "t-p")
	f.on("GET /api/v1/customers/cust1/universes/u-1/pitr_config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{
			{"uuid": "pitr-other", "dbName": "otherdb", "state": "COMPLETE"},
			{"uuid": "pitr-1", "dbName": "pdb", "state": "COMPLETE"},
		})
	})
	f.on("POST /api/v1/customers/cust1/universes/u-1/pitr", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["pitrConfigUUID"] != "pitr-1" || req["restoreTimeInMillis"] != float64(1755000000000) {
			t.Errorf("pitr restore body: %v", req)
		}
		writeJSON(w, 200, map[string]any{"taskUUID": "t-r"})
	})
	taskDone(f, "t-r")
	f.on("DELETE /api/v1/customers/cust1/universes/u-1/pitr_config/pitr-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"taskUUID": "t-d"})
	})
	taskDone(f, "t-d")

	c := f.client()
	uuid, err := c.CreatePitrConfigAndWait(context.Background(), "u-1", "pdb", 4*time.Hour, time.Hour)
	if err != nil || uuid != "pitr-1" {
		t.Fatalf("create pitr: %q, %v", uuid, err)
	}
	if err := c.RestorePitrAndWait(context.Background(), "u-1", "pitr-1", 1755000000000); err != nil {
		t.Fatal(err)
	}
	if err := c.DeletePitrConfig(context.Background(), "u-1", "pitr-1"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBackupScheduleAndWait(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/create_backup_schedule_async", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["scheduleName"] != "s1" || req["schedulingFrequency"] != float64(3600000) {
			t.Errorf("schedule body: %v", req)
		}
		writeJSON(w, 200, map[string]any{"taskUUID": "t-s"})
	})
	taskDone(f, "t-s")
	f.on("POST /api/v1/customers/cust1/schedules/page", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entities": []map[string]any{
			{"scheduleUUID": "sched-1", "scheduleName": "s1", "status": "Active", "frequency": 3600000},
		}})
	})

	uuid, err := f.client().CreateBackupScheduleAndWait(context.Background(), BackupSpec{
		UniverseUUID: "u-1", StorageConfigUUID: "sc-1", Keyspace: "probe",
		ScheduleName: "s1", Frequency: time.Hour,
	})
	if err != nil || uuid != "sched-1" {
		t.Fatalf("schedule: %q, %v", uuid, err)
	}
}

func TestCreateBackupScheduleRejectsMissingNameOrFrequency(t *testing.T) {
	f := newFake()
	defer f.close()
	if _, err := f.client().CreateBackupSchedule(context.Background(), BackupSpec{Keyspace: "x"}); err == nil {
		t.Fatal("want an error without name/frequency")
	}
}

func TestListUniverseBackupsAndSchedulesPage(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/backups/page", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entities": []map[string]any{
			{"backupUUID": "b-1", "scheduleUUID": "sched-1", "commonBackupInfo": map[string]any{"state": BackupStateCompleted}},
		}})
	})
	f.on("POST /api/v1/customers/cust1/schedules/page", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entities": []map[string]any{
			{"scheduleUUID": "sched-1", "scheduleName": "s1", "status": "Active", "frequency": 3600000},
		}})
	})
	c := f.client()
	backups, err := c.ListUniverseBackups(context.Background(), "u-1")
	if err != nil || len(backups) != 1 || backups[0].ScheduleUUID != "sched-1" {
		t.Fatalf("backups = %+v, err %v", backups, err)
	}
	schedules, err := c.ListBackupSchedules(context.Background())
	if err != nil || len(schedules) != 1 || schedules[0].Status != "Active" {
		t.Fatalf("schedules = %+v, err %v", schedules, err)
	}
}

func TestHasYbc(t *testing.T) {
	var u Universe
	if u.HasYbc() {
		t.Error("empty universe reports YBC")
	}
	u.UniverseDetails.EnableYbc = true
	if !u.HasYbc() {
		t.Error("enableYbc=true not reported")
	}
}
