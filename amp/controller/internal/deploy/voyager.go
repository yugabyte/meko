package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	voyagerExportDir = "/voyager/export"

	// VoyagerDeployerKubernetes and VoyagerDeployerDocker identify the runtime
	// represented by a RunRef.
	VoyagerDeployerKubernetes = "kubernetes"
	VoyagerDeployerDocker     = "docker"
)

// VoyagerOperation is the domain operation performed by a Voyager job.
type VoyagerOperation string

const (
	VoyagerOperationAssess  VoyagerOperation = "assess"
	VoyagerOperationMigrate VoyagerOperation = "migrate"
)

// RunState is the runtime state of a submitted Voyager run.
type RunState string

const (
	RunStatePending   RunState = "pending"
	RunStateRunning   RunState = "running"
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"
)

// Terminal reports whether no further runtime transition is expected.
func (s RunState) Terminal() bool {
	return s == RunStateSucceeded || s == RunStateFailed
}

// VoyagerSourceConfig is the source PostgreSQL connection consumed by Voyager.
type VoyagerSourceConfig struct {
	DBType   string   `json:"db_type"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	Database string   `json:"database"`
	Schemas  []string `json:"schemas"`
}

// VoyagerTargetConfig is the target PG compute connection consumed by Voyager.
// The target database need not exist; the migrate command creates it safely.
type VoyagerTargetConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// VoyagerRunSpec describes one immutable migration attempt.
type VoyagerRunSpec struct {
	MigrationID  uuid.UUID            `json:"migration_id"`
	Operation    VoyagerOperation     `json:"operation"`
	Attempt      int                  `json:"attempt"`
	Source       VoyagerSourceConfig  `json:"source"`
	Target       *VoyagerTargetConfig `json:"target,omitempty"`
	ParallelJobs int                  `json:"parallel_jobs"`
}

// RunRef is the deployer-neutral handle persisted with a migration run. Only
// fields for the selected deployer are populated.
//
// TODO(voyager): Replace the flattened runtime fields with deployer-owned opaque
// metadata types when another runtime makes that indirection worthwhile.
type RunRef struct {
	// DeployerType is stored in migration_run.deployer_type; the remaining
	// fields are the JSON deployer_metadata payload.
	DeployerType string `json:"-"`
	Namespace    string `json:"namespace,omitempty"`
	JobName      string `json:"job_name,omitempty"`
	JobUID       string `json:"job_uid,omitempty"`
	ContainerID  string `json:"container_id,omitempty"`
	Volume       string `json:"volume"`
}

// VoyagerJobRunner launches Voyager work and derives its state from the
// selected deployer. StartRun is idempotent for a migration, operation, and
// attempt.
type VoyagerJobRunner interface {
	StartRun(ctx context.Context, spec VoyagerRunSpec) (RunRef, error)
	RunStatus(ctx context.Context, ref RunRef) (RunState, error)
}

type voyagerEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type voyagerJobSpec struct {
	Name        string
	Volume      string
	Env         []voyagerEnvVar
	Command     string
	MigrationID uuid.UUID
}

func buildVoyagerJobSpec(spec VoyagerRunSpec, storeDbConnectionString string) (voyagerJobSpec, error) {
	if err := validateVoyagerRunSpec(spec, storeDbConnectionString); err != nil {
		return voyagerJobSpec{}, err
	}

	target := VoyagerTargetConfig{}
	if spec.Target != nil {
		target = *spec.Target
	}
	// TODO(voyager): Move Voyager job configuration from environment variables
	// to a mounted config file once the Voyager interface supports it.
	env := map[string]string{
		"CONTROL_PLANE_TYPE":          "yugabyted",
		"YUGABYTED_DB_CONN_STRING":    storeDbConnectionString,
		"YB_VOYAGER_MIGRATION_UUID":   spec.MigrationID.String(),
		"EXPORT_DIR":                  voyagerExportDir,
		"YB_VOYAGER_SEND_DIAGNOSTICS": "0",
		"SOURCE_DB_TYPE":              spec.Source.DBType,
		"SOURCE_HOST":                 spec.Source.Host,
		"SOURCE_PORT":                 strconv.Itoa(spec.Source.Port),
		"SOURCE_USER":                 spec.Source.Username,
		"SOURCE_DB":                   spec.Source.Database,
		"SOURCE_SCHEMAS":              strings.Join(spec.Source.Schemas, ","),
		"SOURCE_DB_PASSWORD":          spec.Source.Password,
		"TARGET_HOST":                 target.Host,
		"TARGET_PORT":                 portString(target.Port),
		"TARGET_USER":                 target.Username,
		"TARGET_DB":                   target.Database,
		"TARGET_DB_PASSWORD":          target.Password,
		"PARALLEL_JOBS":               strconv.Itoa(spec.ParallelJobs),
	}

	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	// Maps iterate randomly in Go. Sort to make generated Job specs, logs,
	// tests, and Kubernetes diffs deterministic across equivalent replays.
	sort.Strings(names)
	envVars := make([]voyagerEnvVar, 0, len(names))
	for _, name := range names {
		envVars = append(envVars, voyagerEnvVar{Name: name, Value: env[name]})
	}

	command := assessCommand()
	if spec.Operation == VoyagerOperationMigrate {
		command = migrateCommand()
	}
	return voyagerJobSpec{
		Name:        voyagerRunName(spec.MigrationID, spec.Operation, spec.Attempt),
		Volume:      voyagerVolumeName(spec.MigrationID),
		Env:         envVars,
		Command:     command,
		MigrationID: spec.MigrationID,
	}, nil
}

func validateVoyagerRunSpec(spec VoyagerRunSpec, storeDbConnectionString string) error {
	if spec.MigrationID == uuid.Nil {
		return errors.New("voyager migration id is required")
	}
	if spec.Operation != VoyagerOperationAssess && spec.Operation != VoyagerOperationMigrate {
		return errors.New("voyager operation must be assess or migrate")
	}
	if spec.Attempt < 1 {
		return errors.New("voyager attempt must be positive")
	}
	if storeDbConnectionString == "" {
		return errors.New("voyager control-plane database connection is required")
	}
	if spec.Source.DBType != "postgresql" {
		return errors.New("voyager source database type must be postgresql")
	}
	if spec.Source.Host == "" || spec.Source.Port < 1 || spec.Source.Username == "" ||
		spec.Source.Database == "" || len(spec.Source.Schemas) == 0 {
		return errors.New("voyager source connection is incomplete")
	}
	for _, schema := range spec.Source.Schemas {
		if schema == "" {
			return errors.New("voyager source schema must not be empty")
		}
	}
	if spec.Operation == VoyagerOperationMigrate {
		if spec.Target == nil {
			return errors.New("voyager target connection is required for migrate")
		}
		if spec.Target.Host == "" || spec.Target.Port < 1 || spec.Target.Username == "" ||
			spec.Target.Database == "" {
			return errors.New("voyager target connection is incomplete")
		}
		if spec.ParallelJobs < 1 {
			return errors.New("voyager parallel jobs must be positive for migrate")
		}
	}
	return nil
}

func voyagerVolumeName(migrationID uuid.UUID) string {
	return "voyager-exp-" + migrationID.String()[:8]
}

func voyagerRunName(migrationID uuid.UUID, operation VoyagerOperation, attempt int) string {
	return fmt.Sprintf("voyager-%s-%s-a%d", operation, migrationID.String()[:8], attempt)
}

func portString(port int) string {
	if port == 0 {
		return ""
	}
	return strconv.Itoa(port)
}

func assessCommand() string {
	return `set -euo pipefail
yb-voyager assess-migration \
  --source-db-type "$SOURCE_DB_TYPE" \
  --source-db-host "$SOURCE_HOST" --source-db-port "$SOURCE_PORT" \
  --source-db-user "$SOURCE_USER" --source-db-name "$SOURCE_DB" \
  --source-db-schema "$SOURCE_SCHEMAS" \
  --export-dir "$EXPORT_DIR" \
  --iops-capture-interval 0 \
  --yes`
}

// TODO(voyager): Verify and implement Voyager's required schema rerun flags
// before promising that a failed migrate command can resume from this shared
// export directory. A rerun may otherwise fail at a schema step before data
// import gets a chance to resume.
func migrateCommand() string {
	return `set -euo pipefail

yb-voyager export schema \
  --source-db-type "$SOURCE_DB_TYPE" \
  --source-db-host "$SOURCE_HOST" --source-db-port "$SOURCE_PORT" \
  --source-db-user "$SOURCE_USER" --source-db-name "$SOURCE_DB" \
  --source-db-schema "$SOURCE_SCHEMAS" \
  --export-dir "$EXPORT_DIR" --yes

PGPASSWORD="$TARGET_DB_PASSWORD" psql \
  -h "$TARGET_HOST" -p "$TARGET_PORT" -U "$TARGET_USER" -d postgres \
  --set=ON_ERROR_STOP=1 --set=target_db="$TARGET_DB" <<'VOYAGER_SQL'
SELECT format('CREATE DATABASE %I', :'target_db')
WHERE NOT EXISTS (
  SELECT 1 FROM pg_database WHERE datname = :'target_db'
)
\gexec
VOYAGER_SQL

yb-voyager import schema \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --export-dir "$EXPORT_DIR" --yes

yb-voyager export data \
  --source-db-type "$SOURCE_DB_TYPE" \
  --source-db-host "$SOURCE_HOST" --source-db-port "$SOURCE_PORT" \
  --source-db-user "$SOURCE_USER" --source-db-name "$SOURCE_DB" \
  --source-db-schema "$SOURCE_SCHEMAS" \
  --export-dir "$EXPORT_DIR" --yes

yb-voyager import data \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --parallel-jobs "$PARALLEL_JOBS" \
  --export-dir "$EXPORT_DIR" --yes

yb-voyager finalize-schema-post-data-import \
  --target-db-type yugabytedb-amp \
  --target-db-host "$TARGET_HOST" --target-db-port "$TARGET_PORT" \
  --target-db-user "$TARGET_USER" --target-db-name "$TARGET_DB" \
  --export-dir "$EXPORT_DIR" --yes`
}
