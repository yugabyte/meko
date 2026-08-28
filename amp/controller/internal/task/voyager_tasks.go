package task

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/voyager/migration"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

// This file holds the Voyager-migration task planners, split out of the generic
// task surface (tasks.go) so the Voyager operations live together. Each planner
// generates its ids up front and describes the ordered steps as StepSpecs,
// exactly like the core planners; the launcher/target steps that execute them
// live in the voyager_step_*.go files.

// Voyager task kinds — the task.kind values for the migration operations.
const (
	KindVoyagerStartMigrationRun     = "start_migration_run"
	KindVoyagerCreateMigrationTarget = "create_migration_target"
)

// VoyagerStartMigrationRunInput is the immutable launch request persisted in
// task params. Attempt and ParallelJobs are selected before planning, so the
// plan is fixed and every replay addresses the same run row and deployer job.
type VoyagerStartMigrationRunInput struct {
	MigrationID  uuid.UUID           `json:"migration_id"`
	Operation    migration.Operation `json:"operation"`
	Attempt      int                 `json:"attempt"`
	ParallelJobs int                 `json:"parallel_jobs"`
}

// VoyagerStartMigrationRun records the two-step launcher backing POST /assess
// and POST /migrate: first write-ahead the durable run row (ensure_migration_run),
// then idempotently launch Voyager and mark that run running (launch_voyager_job).
// The run id is derived deterministically from the migration, operation, and
// attempt, so duplicate launcher tasks — two callers both passing the handler's
// cheap active-run precheck — converge on one run row and one deployer job rather
// than racing into separate resources. The task id is fixed write-ahead so both
// steps fence their run transitions on the same owning task.
func (s *Service) VoyagerStartMigrationRun(ctx context.Context, in VoyagerStartMigrationRunInput) (*Task, error) {
	taskID := uuid.New()
	runID := uuid.NewSHA1(
		in.MigrationID,
		[]byte(fmt.Sprintf("voyager-run:%s:%d", in.Operation, in.Attempt)),
	)
	steps := []StepSpec{
		stepSpec(StepVoyagerEnsureRun, VoyagerEnsureRunArgs{
			RunID:       runID,
			MigrationID: in.MigrationID,
			Operation:   in.Operation,
			Attempt:     in.Attempt,
			TaskID:      taskID,
		}),
		stepSpec(StepVoyagerLaunchJob, VoyagerLaunchJobArgs{
			RunID:        runID,
			MigrationID:  in.MigrationID,
			Operation:    in.Operation,
			Attempt:      in.Attempt,
			TaskID:       taskID,
			ParallelJobs: in.ParallelJobs,
		}),
	}
	return s.createWithID(ctx, taskID, KindVoyagerStartMigrationRun, in, uuid.Nil, in.MigrationID.String(), steps)
}

// voyagerTargetPgVersion is the Postgres major the migration target compute is
// provisioned at. AMP deploys v18-only, so the target the migrate job imports
// into is a PG18-compatible YugabyteDB endpoint.
const voyagerTargetPgVersion = 18

// VoyagerCreateMigrationTargetInput is the write-ahead input for provisioning a
// migration's PG compute. TaskID, when set by the initiating handler (which also
// claims target provisioning via SetTargetTask), is honored so the attach step
// fences on the claim holder; zero defers to a fresh id. TargetDatabaseName is
// the name to give the provisioned target database (carried from the source so
// the migrate job imports into a like-named database).
type VoyagerCreateMigrationTargetInput struct {
	TaskID             uuid.UUID `json:"-"`
	MigrationID        uuid.UUID `json:"migration_id"`
	PoolID             uuid.UUID `json:"pool_id"`
	ProjectName        string    `json:"project_name"`
	TargetDatabaseName string    `json:"target_database_name"`
	VCPU               int       `json:"vcpu"`
	// MemoryGiBPerVCPU is the control plane's memory-per-core shape, passed in
	// because config belongs to the caller. 0 = the built-in.
	MemoryGiBPerVCPU int `json:"memory_gib_per_vcpu,omitempty"`
}

// VoyagerCreateMigrationTarget records a target-provisioning task: it reuses the
// existing project/branch/endpoint steps to allocate a right-sized compute, then
// appends an attach step that records it as the migration's target. All resource
// ids, including the task id, are fixed before step args are serialized so replay
// always addresses the same project, branch, endpoint, and migration. The
// endpoint is sized by VCPU via the create-endpoint step's CPU quantity (zero
// leaves the chart default).
func (s *Service) VoyagerCreateMigrationTarget(ctx context.Context, in VoyagerCreateMigrationTargetInput) (*Task, error) {
	taskID := in.TaskID
	if taskID == uuid.Nil {
		taskID = uuid.New()
	}
	clusterID := uuid.New()
	branchID := uuid.New()
	endpointID := uuid.New()

	// Through the same rule the API applies: a migration target sized only by
	// vCPU would otherwise reach the deployer with no memory and land on its
	// fallback, which for a 16-core target is 4Gi — the shape naming CPU alone is
	// supposed to make impossible.
	var cpu string
	if in.VCPU > 0 {
		cpu = strconv.Itoa(in.VCPU)
	}
	cpu, memory, shapeErr := deploy.ResolveEndpointShape(cpu, "", in.MemoryGiBPerVCPU)
	if shapeErr != "" {
		return nil, fmt.Errorf("migration target size: %s", shapeErr)
	}

	steps := []StepSpec{
		stepSpec(StepCreateCluster, CreateClusterArgs{
			PoolID:    in.PoolID,
			ClusterID: clusterID,
			BranchID:  branchID,
			Name:      in.ProjectName,
			PgVersion: voyagerTargetPgVersion,
		}),
		stepSpec(StepRecordBranchRecipe, RecordBranchRecipeArgs{
			ClusterID:  clusterID,
			BranchID:   branchID,
			BranchName: defaultBranchName,
			PgVersion:  voyagerTargetPgVersion,
			StartLSN:   "0/0",
		}),
		stepSpec(StepCreateEndpoint, CreateEndpointArgs{
			EndpointID: endpointID,
			ClusterID:  clusterID,
			BranchID:   branchID,
			Role:       roleReadWrite,
			PgVersion:  voyagerTargetPgVersion,
			CPU:        cpu,
			Memory:     memory,
		}),
		stepSpec(StepVoyagerAttachTarget, VoyagerAttachTargetArgs{
			MigrationID:        in.MigrationID,
			TaskID:             taskID,
			PoolID:             in.PoolID,
			ClusterID:          clusterID,
			BranchID:           branchID,
			EndpointID:         endpointID,
			TargetDatabaseName: in.TargetDatabaseName,
		}),
	}
	return s.createWithID(ctx, taskID, KindVoyagerCreateMigrationTarget, in, clusterID, in.MigrationID.String(), steps)
}
