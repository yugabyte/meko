// Package migration contains the Voyager migration domain model. It is kept
// independent of persistence, deployment, and HTTP so the state machine can be
// used and tested in isolation.
package migration

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound           = errors.New("migration resource not found")
	ErrConflict           = errors.New("migration state conflict")
	ErrAssessmentNotFound = errors.New("migration assessment not found")
)

// SourceConfig is retained internally so Voyager jobs can connect to the
// source. API projections must never expose Password.
type SourceConfig struct {
	DBType   string   `json:"db_type"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	Database string   `json:"database"`
	Schemas  []string `json:"schemas"`
}

// TargetConfig is retained internally so Voyager jobs can connect to the AMP
// target. API projections must never expose Password.
type TargetConfig struct {
	PoolID     uuid.UUID `json:"pool_id"`
	ClusterID  uuid.UUID `json:"cluster_id"`
	BranchID   uuid.UUID `json:"branch_id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Username   string    `json:"username"`
	Password   string    `json:"password"`
	Database   string    `json:"database"`
}

type Operation string

const (
	OperationAssess  Operation = "assess"
	OperationMigrate Operation = "migrate"
	OperationEnd     Operation = "end"
)

func (o Operation) Valid() bool {
	return o == OperationAssess || o == OperationMigrate || o == OperationEnd
}

type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
)

func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed
}

func (s RunStatus) InFlight() bool {
	return s == RunPending || s == RunRunning
}

// Run is one assess, migrate, or end attempt. DeployerMetadata intentionally
// stores only a safe runner reference, never source or target credentials.
type Run struct {
	ID               uuid.UUID       `json:"id"`
	MigrationID      uuid.UUID       `json:"migration_id"`
	Operation        Operation       `json:"operation"`
	Attempt          int             `json:"attempt"`
	Status           RunStatus       `json:"status"`
	ErrorSummary     *string         `json:"error_summary,omitempty"`
	DeployerType     *string         `json:"deployer_type,omitempty"`
	DeployerMetadata json.RawMessage `json:"deployer_metadata,omitempty"`
	TaskID           *uuid.UUID      `json:"task_id,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// RunSpec is the write-ahead identity fixed when a launcher task is planned.
type RunSpec struct {
	ID          uuid.UUID
	MigrationID uuid.UUID
	Operation   Operation
	Attempt     int
	TaskID      *uuid.UUID
}

type MigrationState string

const (
	StateCreated            MigrationState = "created"
	StateAssessing          MigrationState = "assessing"
	StateAssessed           MigrationState = "assessed"
	StateProvisioningTarget MigrationState = "provisioning_target"
	StateTargetReady        MigrationState = "target_ready"
	StateMigrating          MigrationState = "migrating"
	StateCompleted          MigrationState = "completed"
	StateEnding             MigrationState = "ending"
	StateEnded              MigrationState = "ended"
	StateFailed             MigrationState = "failed"
)

type Status struct {
	State  MigrationState `json:"state"`
	Detail string         `json:"detail"`
}

type Migration struct {
	ID           uuid.UUID      `json:"id"`
	Name         string         `json:"name"`
	Source       SourceConfig   `json:"source"`
	Target       *TargetConfig  `json:"target,omitempty"`
	TargetTaskID *uuid.UUID     `json:"target_task_id,omitempty"`
	Runs         []Run          `json:"runs"`
	Voyager      *VoyagerStatus `json:"voyager,omitempty"`
	State        MigrationState `json:"state"`
	Detail       string         `json:"detail"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type MigrationSummary struct {
	ID        uuid.UUID      `json:"id"`
	Name      string         `json:"name"`
	State     MigrationState `json:"state"`
	Detail    string         `json:"detail"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type PhaseNumber int

// Phase numbers are Voyager's fixed identifiers — the integers it writes into
// ybvoyager_visualizer_metadata.migration_phase — not a sequence, so the values
// are non-contiguous read top to bottom. They are declared in workflow order
// (assess -> export-schema -> analyze-schema -> import-schema -> export-data ->
// import-data); the same order is applied to status reads by the ORDER BY in
// visualizer.go. import-schema (5) precedes export-data (4) because the migrate
// job imports schema before it exports data.
const (
	PhaseAssess       PhaseNumber = 1
	PhaseExportSchema PhaseNumber = 2
	PhaseAnalyze      PhaseNumber = 3
	PhaseImportSchema PhaseNumber = 5
	PhaseExportData   PhaseNumber = 4
	PhaseImportData   PhaseNumber = 6
)

func (p PhaseNumber) Name() string {
	switch p {
	case PhaseAssess:
		return "assess"
	case PhaseExportSchema:
		return "export-schema"
	case PhaseAnalyze:
		return "analyze-schema"
	case PhaseExportData:
		return "export-data"
	case PhaseImportSchema:
		return "import-schema"
	case PhaseImportData:
		return "import-data"
	default:
		return ""
	}
}

type PhaseStatus string

const (
	PhaseNotStarted PhaseStatus = "not_started"
	PhaseInProgress PhaseStatus = "in_progress"
	PhaseCompleted  PhaseStatus = "completed"
)

type Phase struct {
	Phase     PhaseNumber `json:"phase"`
	Name      string      `json:"name"`
	Status    PhaseStatus `json:"status"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type TableStatus string

const (
	TableNotStarted TableStatus = "not_started"
	TableInProgress TableStatus = "in_progress"
	TableDone       TableStatus = "done"
	TableCompleted  TableStatus = "completed"
)

type TableProgress struct {
	Schema        string      `json:"schema"`
	Table         string      `json:"table"`
	Status        TableStatus `json:"status"`
	RowsCompleted int64       `json:"rows_completed"`
	RowsTotal     int64       `json:"rows_total"`
}

type PhaseDataProgress struct {
	RowsCompleted int64           `json:"rows_completed"`
	RowsTotal     int64           `json:"rows_total"`
	Tables        []TableProgress `json:"tables"`
}

type DataProgress struct {
	Export *PhaseDataProgress `json:"export,omitempty"`
	Import *PhaseDataProgress `json:"import,omitempty"`
}

type VoyagerStatus struct {
	Phases       []Phase       `json:"phases"`
	DataProgress *DataProgress `json:"data_progress,omitempty"`
}

// Assessment is the agent-oriented projection of Voyager's completed
// AssessMigrationPayloadYugabyteD payload.
type Assessment struct {
	VoyagerVersion                 string               `json:"voyager_version"`
	MigrationComplexity            string               `json:"migration_complexity"`
	MigrationComplexityExplanation string               `json:"migration_complexity_explanation"`
	SourceEnvironment              SourceEnvironment    `json:"source_environment"`
	SourceDatabase                 SourceDBStats        `json:"source_database"`
	SizingRecommendation           SizingRecommendation `json:"sizing_recommendation"`
	Issues                         []AssessmentIssue    `json:"issues"`
	Notes                          []string             `json:"notes"`
}

// SourceEnvironment is reserved by the API projection. Voyager payload v1.8
// does not currently populate these fields, so they are normally zero.
type SourceEnvironment struct {
	TotalVCPU     float64 `json:"total_vcpu"`
	TotalMemory   int64   `json:"total_memory"`
	TotalDiskSize int64   `json:"total_disk_size"`
}

// SourceDBStats contains size and row-count facts from the source.
type SourceDBStats struct {
	TotalTableSizeBytes int64 `json:"total_table_size_bytes"`
	TotalIndexSizeBytes int64 `json:"total_index_size_bytes"`
	TableRowCount       int64 `json:"table_row_count"`
}

// SizingRecommendation is the compute-sizing subset relevant to the agent.
type SizingRecommendation struct {
	NumNodes        float64  `json:"num_nodes"`
	VCPUPerNode     int      `json:"vcpu_per_node"`
	MemoryPerNodeGB int      `json:"memory_per_node_gb"`
	ColocatedTables []string `json:"colocated_tables"`
	ShardedTables   []string `json:"sharded_tables"`
	Summary         string   `json:"summary"`
}

// AssessmentIssue is the flattened, agent-readable issue projection.
type AssessmentIssue struct {
	Category    string `json:"category"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Impact      string `json:"impact"`
	ObjectType  string `json:"object_type"`
	ObjectName  string `json:"object_name"`
	DocsLink    string `json:"docs_link"`
}
