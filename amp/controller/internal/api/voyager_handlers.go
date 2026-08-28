package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// This file holds the Voyager migration API handlers. Reads derive status over
// the migration store/service; mutations (assess/target/migrate) run a cheap
// synchronous pre-check, then plan a write-ahead task and return 202 + the Task +
// a Location header, exactly like the project/branch/endpoint mutations. The
// feature is gated: when the controller is started without a Voyager database
// (prov.VoyagerMigrations == nil) every route refuses with 501.

// defaultVoyagerParallelJobs is the fixed default for `import data
// --parallel-jobs`, used when a migrate request omits parallel_jobs. Hardcoded
// for now; there is no operator-level override.
const defaultVoyagerParallelJobs = 8

// migrationAPIDisabled is the 501 returned when the Voyager migration feature is
// not configured on this controller. It renders through the standard Error
// envelope like any other echo.HTTPError.
func migrationAPIDisabled() error {
	return echo.NewHTTPError(http.StatusNotImplemented, "migration APIs are not configured on this controller")
}

func (c *Controllers) CheckMigrationConnectivity(ctx context.Context, request CheckMigrationConnectivityRequestObject) (CheckMigrationConnectivityResponseObject, error) {
	if c.prov.VoyagerMigrations == nil {
		return nil, migrationAPIDisabled()
	}
	if request.Body == nil {
		return CheckMigrationConnectivity400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	source, msg := sourceFromRequest(request.Body.Source)
	if msg != "" {
		return CheckMigrationConnectivity400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", msg))}, nil
	}
	// A source that can't be reached is a successful probe (reachable=false),
	// not an error — the redacted reason travels in the body.
	result := migration.CheckConnectivity(ctx, source)
	return CheckMigrationConnectivity200JSONResponse(connectivityToAPI(result)), nil
}

func (c *Controllers) CreateMigration(ctx context.Context, request CreateMigrationRequestObject) (CreateMigrationResponseObject, error) {
	if c.prov.VoyagerMigrations == nil {
		return nil, migrationAPIDisabled()
	}
	if request.Body == nil {
		return CreateMigration400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	if strings.TrimSpace(request.Body.Name) == "" {
		return CreateMigration400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "migration name is required"))}, nil
	}
	source, msg := sourceFromRequest(request.Body.Source)
	if msg != "" {
		return CreateMigration400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", msg))}, nil
	}
	// The migration id is the idempotency key (also injected into Voyager as its
	// migration_uuid); the store converges a replay onto the existing row.
	item, err := c.prov.VoyagerMigrations.CreateMigration(ctx, uuid.New(), request.Body.Name, source)
	if err != nil {
		return nil, fmt.Errorf("create migration: %w", err)
	}
	out, err := migrationToAPI(item)
	if err != nil {
		return nil, err
	}
	return CreateMigration201JSONResponse(out), nil
}

func (c *Controllers) ListMigrations(ctx context.Context, request ListMigrationsRequestObject) (ListMigrationsResponseObject, error) {
	if c.prov.VoyagerMigrations == nil {
		return nil, migrationAPIDisabled()
	}
	limit, offset := pageParams(request.Params.Limit, request.Params.Offset)
	items, total, err := c.prov.VoyagerMigrations.ListMigrations(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	out := make([]MigrationSummary, 0, len(items))
	for _, s := range items {
		out = append(out, summaryToAPI(s))
	}
	return ListMigrations200JSONResponse(MigrationList{Migrations: out, Total: total, Count: len(out)}), nil
}

func (c *Controllers) GetMigration(ctx context.Context, request GetMigrationRequestObject) (GetMigrationResponseObject, error) {
	if c.prov.VoyagerMigrationService == nil {
		return nil, migrationAPIDisabled()
	}
	// Derive-on-read: refresh in-flight runs from the deployer, persist terminal
	// transitions, and re-derive state from the durable rows.
	item, err := c.prov.VoyagerMigrationService.Get(ctx, request.MigrationId)
	switch {
	case errors.Is(err, migration.ErrNotFound):
		return GetMigration404JSONResponse{NotFoundJSONResponse(apiError("not_found", "migration not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get migration: %w", err)
	}
	out, err := migrationToAPI(item)
	if err != nil {
		return nil, err
	}
	return GetMigration200JSONResponse(out), nil
}

func (c *Controllers) GetMigrationAssessment(ctx context.Context, request GetMigrationAssessmentRequestObject) (GetMigrationAssessmentResponseObject, error) {
	if c.prov.VoyagerMigrations == nil {
		return nil, migrationAPIDisabled()
	}
	a, err := c.prov.VoyagerMigrations.GetAssessment(ctx, request.MigrationId)
	switch {
	case errors.Is(err, migration.ErrNotFound):
		return GetMigrationAssessment404JSONResponse{NotFoundJSONResponse(apiError("not_found", "migration not found"))}, nil
	case errors.Is(err, migration.ErrAssessmentNotFound):
		return GetMigrationAssessment404JSONResponse{NotFoundJSONResponse(apiError("not_found", "no completed assessment for this migration"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get migration assessment: %w", err)
	}
	return GetMigrationAssessment200JSONResponse(assessmentToAPI(a)), nil
}

func (c *Controllers) StartMigrationAssessment(ctx context.Context, request StartMigrationAssessmentRequestObject) (StartMigrationAssessmentResponseObject, error) {
	if c.prov.VoyagerMigrations == nil || c.prov.VoyagerRunner == nil {
		return nil, migrationAPIDisabled()
	}
	if _, err := c.prov.VoyagerMigrations.GetMigration(ctx, request.MigrationId); err != nil {
		if errors.Is(err, migration.ErrNotFound) {
			return StartMigrationAssessment404JSONResponse{NotFoundJSONResponse(apiError("not_found", "migration not found"))}, nil
		}
		return nil, fmt.Errorf("get migration: %w", err)
	}
	active, err := c.prov.VoyagerMigrations.HasActiveRun(ctx, request.MigrationId)
	if err != nil {
		return nil, fmt.Errorf("check migration run: %w", err)
	}
	if active {
		return StartMigrationAssessment409JSONResponse{ConflictJSONResponse(apiError("run_in_progress", "another migration run is already in progress"))}, nil
	}
	attempt, err := c.prov.VoyagerMigrations.NextAttempt(ctx, request.MigrationId, migration.OperationAssess)
	if err != nil {
		return nil, fmt.Errorf("get migration attempt: %w", err)
	}
	t, err := c.tasks.VoyagerStartMigrationRun(ctx, task.VoyagerStartMigrationRunInput{
		MigrationID: request.MigrationId,
		Operation:   migration.OperationAssess,
		Attempt:     attempt,
	})
	if err != nil {
		return nil, fmt.Errorf("start assessment task: %w", err)
	}
	return StartMigrationAssessment202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

func (c *Controllers) StartMigration(ctx context.Context, request StartMigrationRequestObject) (StartMigrationResponseObject, error) {
	if c.prov.VoyagerMigrations == nil || c.prov.VoyagerRunner == nil {
		return nil, migrationAPIDisabled()
	}
	item, err := c.prov.VoyagerMigrations.GetMigration(ctx, request.MigrationId)
	switch {
	case errors.Is(err, migration.ErrNotFound):
		return StartMigration404JSONResponse{NotFoundJSONResponse(apiError("not_found", "migration not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get migration: %w", err)
	}
	if item.Target == nil {
		return StartMigration409JSONResponse{ConflictJSONResponse(apiError("target_not_attached", "migration target is not attached"))}, nil
	}
	active, err := c.prov.VoyagerMigrations.HasActiveRun(ctx, request.MigrationId)
	if err != nil {
		return nil, fmt.Errorf("check migration run: %w", err)
	}
	if active {
		return StartMigration409JSONResponse{ConflictJSONResponse(apiError("run_in_progress", "another migration run is already in progress"))}, nil
	}
	parallelJobs := defaultVoyagerParallelJobs
	if request.Body != nil && request.Body.ParallelJobs != nil {
		parallelJobs = *request.Body.ParallelJobs
	}
	if parallelJobs < 1 {
		return StartMigration400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "parallel_jobs must be positive"))}, nil
	}
	attempt, err := c.prov.VoyagerMigrations.NextAttempt(ctx, request.MigrationId, migration.OperationMigrate)
	if err != nil {
		return nil, fmt.Errorf("get migration attempt: %w", err)
	}
	t, err := c.tasks.VoyagerStartMigrationRun(ctx, task.VoyagerStartMigrationRunInput{
		MigrationID:  request.MigrationId,
		Operation:    migration.OperationMigrate,
		Attempt:      attempt,
		ParallelJobs: parallelJobs,
	})
	if err != nil {
		return nil, fmt.Errorf("start migration task: %w", err)
	}
	return StartMigration202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

func (c *Controllers) ProvisionMigrationTarget(ctx context.Context, request ProvisionMigrationTargetRequestObject) (ProvisionMigrationTargetResponseObject, error) {
	// Provisioning reuses the project/endpoint deployer, so it also requires the
	// endpoint deployer to be configured.
	if c.prov.VoyagerMigrations == nil || c.prov.Deployer == nil {
		return nil, migrationAPIDisabled()
	}
	if request.Body == nil {
		return ProvisionMigrationTarget400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "request body is required"))}, nil
	}
	if request.Body.PoolId == uuid.Nil {
		return ProvisionMigrationTarget400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "pool_id is required"))}, nil
	}
	vcpu := 0
	if request.Body.Vcpu != nil {
		vcpu = *request.Body.Vcpu
		if vcpu < 1 {
			return ProvisionMigrationTarget400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "vcpu must be at least 1"))}, nil
		}
	}
	if _, err := c.backend.GetStoragePool(ctx, request.Body.PoolId); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return ProvisionMigrationTarget404JSONResponse{NotFoundJSONResponse(apiError("not_found", "storage pool not found"))}, nil
		}
		return nil, fmt.Errorf("get storage pool: %w", err)
	}
	item, err := c.prov.VoyagerMigrations.GetMigration(ctx, request.MigrationId)
	switch {
	case errors.Is(err, migration.ErrNotFound):
		return ProvisionMigrationTarget404JSONResponse{NotFoundJSONResponse(apiError("not_found", "migration not found"))}, nil
	case err != nil:
		return nil, fmt.Errorf("get migration: %w", err)
	}
	if item.Target != nil {
		return ProvisionMigrationTarget409JSONResponse{ConflictJSONResponse(apiError("target_conflict", "migration target is already attached"))}, nil
	}
	// A prior claim only blocks a new one while its task is still live; a failed
	// (or vanished) provision leaves a stale claim we release so the caller can
	// retry.
	if item.TargetTaskID != nil {
		prev, err := c.tasks.Get(ctx, *item.TargetTaskID)
		if err != nil {
			return nil, fmt.Errorf("get target task: %w", err)
		}
		if prev != nil && prev.Status != task.StatusFailed {
			return ProvisionMigrationTarget409JSONResponse{ConflictJSONResponse(apiError("target_conflict", "a target provisioning task is already in progress"))}, nil
		}
		if err := c.prov.VoyagerMigrations.ClearTargetTask(ctx, request.MigrationId, *item.TargetTaskID); err != nil {
			return nil, fmt.Errorf("clear stale target task: %w", err)
		}
	}
	projectName := item.Name
	if request.Body.ProjectName != nil && strings.TrimSpace(*request.Body.ProjectName) != "" {
		projectName = *request.Body.ProjectName
	}
	// Claim provisioning for this task id before planning so status can report
	// provisioning_target and a competing request is rejected.
	taskID := uuid.New()
	if err := c.prov.VoyagerMigrations.SetTargetTask(ctx, request.MigrationId, taskID); err != nil {
		if errors.Is(err, migration.ErrConflict) {
			return ProvisionMigrationTarget409JSONResponse{ConflictJSONResponse(apiError("target_conflict", "a target provisioning task is already in progress"))}, nil
		}
		return nil, fmt.Errorf("claim target provisioning: %w", err)
	}
	t, err := c.tasks.VoyagerCreateMigrationTarget(ctx, task.VoyagerCreateMigrationTargetInput{
		MemoryGiBPerVCPU:   c.memoryFloorGiBPerVCPU(),
		TaskID:             taskID,
		MigrationID:        request.MigrationId,
		PoolID:             request.Body.PoolId,
		ProjectName:        projectName,
		TargetDatabaseName: item.Source.Database,
		VCPU:               vcpu,
	})
	if err != nil {
		// Release the claim so a retry isn't wedged behind a task that never got planned.
		_ = c.prov.VoyagerMigrations.ClearTargetTask(ctx, request.MigrationId, taskID)
		return nil, fmt.Errorf("plan create target task: %w", err)
	}
	return ProvisionMigrationTarget202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}

// --- DTO mapping -----------------------------------------------------------
// The response types intentionally have no password field; these mappers are the
// single place the domain → wire projection happens, so a credential can never
// leak into a response.

// sourceFromRequest validates a source request and builds the internal
// SourceConfig. It returns a non-empty message when the request is invalid (the
// generated types don't enforce required/enum, so the handler validates).
func sourceFromRequest(req MigrationSourceRequest) (migration.SourceConfig, string) {
	if req.DbType != MigrationSourceRequestDbTypePostgresql {
		return migration.SourceConfig{}, "source db_type must be postgresql"
	}
	if strings.TrimSpace(req.Host) == "" || strings.TrimSpace(req.Username) == "" ||
		req.Password == "" || strings.TrimSpace(req.Database) == "" {
		return migration.SourceConfig{}, "source host, username, password, and database are required"
	}
	if req.Port < 1 || req.Port > 65535 {
		return migration.SourceConfig{}, "source port must be between 1 and 65535"
	}
	// schemas is a required, explicit list (matching the Voyager CLI): the caller
	// must name the schemas to migrate rather than relying on an implicit default.
	schemas := append([]string{}, req.Schemas...)
	if len(schemas) == 0 {
		return migration.SourceConfig{}, "source schemas are required (list at least one schema, e.g. [\"public\"])"
	}
	for _, s := range schemas {
		if strings.TrimSpace(s) == "" {
			return migration.SourceConfig{}, "source schemas must not contain empty entries"
		}
	}
	return migration.SourceConfig{
		DBType:   string(req.DbType),
		Host:     req.Host,
		Port:     req.Port,
		Username: req.Username,
		Password: req.Password,
		Database: req.Database,
		Schemas:  schemas,
	}, ""
}

func connectivityToAPI(r migration.ConnectivityResult) MigrationConnectivity {
	return MigrationConnectivity{
		Reachable:     r.Reachable,
		ServerVersion: r.ServerVersion,
		Ssl:           r.SSL,
		LatencyMs:     r.LatencyMS,
		Error:         r.Error,
	}
}

func summaryToAPI(s migration.MigrationSummary) MigrationSummary {
	return MigrationSummary{
		Id:        s.ID,
		Name:      s.Name,
		State:     MigrationState(s.State),
		Detail:    s.Detail,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

func migrationToAPI(item migration.Migration) (Migration, error) {
	out := Migration{
		Id:        item.ID,
		Name:      item.Name,
		State:     MigrationState(item.State),
		Detail:    item.Detail,
		Source:    sourceToAPI(item.Source),
		Runs:      make([]MigrationRun, 0, len(item.Runs)),
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
	if item.Target != nil {
		out.Target = targetToAPI(*item.Target)
	}
	for _, r := range item.Runs {
		run, err := runToAPI(r)
		if err != nil {
			return Migration{}, err
		}
		out.Runs = append(out.Runs, run)
	}
	if item.Voyager != nil {
		out.Voyager = voyagerToAPI(item.Voyager)
	}
	return out, nil
}

func sourceToAPI(s migration.SourceConfig) MigrationSource {
	out := MigrationSource{
		DbType:   MigrationSourceDbType(s.DBType),
		Host:     s.Host,
		Port:     s.Port,
		Username: s.Username,
		Database: s.Database,
	}
	if len(s.Schemas) > 0 {
		schemas := append([]string(nil), s.Schemas...)
		out.Schemas = &schemas
	}
	return out
}

func targetToAPI(t migration.TargetConfig) *MigrationTarget {
	return &MigrationTarget{
		PoolId:     t.PoolID,
		ClusterId:  t.ClusterID,
		EndpointId: t.EndpointID,
		Host:       t.Host,
		Port:       t.Port,
		Username:   t.Username,
		Database:   t.Database,
	}
}

func runToAPI(r migration.Run) (MigrationRun, error) {
	out := MigrationRun{
		Operation:    MigrationRunOperation(r.Operation),
		Attempt:      r.Attempt,
		Status:       MigrationRunStatus(r.Status),
		DeployerType: r.DeployerType,
		ErrorSummary: r.ErrorSummary,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		TaskId:       r.TaskID,
	}
	if len(r.DeployerMetadata) > 0 {
		var meta MigrationRunDeployerMetadata
		if err := json.Unmarshal(r.DeployerMetadata, &meta); err != nil {
			return MigrationRun{}, fmt.Errorf("decode deployer metadata for run %s: %w", r.ID, err)
		}
		out.DeployerMetadata = &meta
	}
	return out, nil
}

func voyagerToAPI(v *migration.VoyagerStatus) *MigrationVoyager {
	out := &MigrationVoyager{Phases: make([]MigrationVoyagerPhase, 0, len(v.Phases))}
	for _, p := range v.Phases {
		updatedAt := p.UpdatedAt
		out.Phases = append(out.Phases, MigrationVoyagerPhase{
			Phase:     int(p.Phase),
			Name:      p.Name,
			Status:    MigrationVoyagerPhaseStatus(p.Status),
			UpdatedAt: &updatedAt,
		})
	}
	if v.DataProgress != nil {
		out.DataProgress = dataProgressToAPI(v.DataProgress)
	}
	return out
}

func dataProgressToAPI(d *migration.DataProgress) *MigrationDataProgress {
	out := &MigrationDataProgress{}
	if d.Export != nil {
		out.Export = phaseProgressToAPI(d.Export)
	}
	if d.Import != nil {
		out.Import = phaseProgressToAPI(d.Import)
	}
	return out
}

func phaseProgressToAPI(p *migration.PhaseDataProgress) *MigrationPhaseDataProgress {
	out := &MigrationPhaseDataProgress{
		RowsCompleted: p.RowsCompleted,
		RowsTotal:     p.RowsTotal,
		Tables:        make([]MigrationTableProgress, 0, len(p.Tables)),
	}
	for _, t := range p.Tables {
		out.Tables = append(out.Tables, MigrationTableProgress{
			Schema:        t.Schema,
			Table:         t.Table,
			Status:        MigrationTableProgressStatus(t.Status),
			RowsCompleted: t.RowsCompleted,
			RowsTotal:     t.RowsTotal,
		})
	}
	return out
}

func assessmentToAPI(a migration.Assessment) MigrationAssessment {
	out := MigrationAssessment{
		VoyagerVersion:                 a.VoyagerVersion,
		MigrationComplexity:            a.MigrationComplexity,
		MigrationComplexityExplanation: a.MigrationComplexityExplanation,
		SourceEnvironment: MigrationAssessmentSourceEnvironment{
			TotalVcpu:     ptrOf(float32(a.SourceEnvironment.TotalVCPU)),
			TotalMemory:   ptrOf(a.SourceEnvironment.TotalMemory),
			TotalDiskSize: ptrOf(a.SourceEnvironment.TotalDiskSize),
		},
		SourceDatabase: MigrationAssessmentSourceDatabaseStats{
			TotalTableSizeBytes: ptrOf(a.SourceDatabase.TotalTableSizeBytes),
			TotalIndexSizeBytes: ptrOf(a.SourceDatabase.TotalIndexSizeBytes),
			TableRowCount:       ptrOf(a.SourceDatabase.TableRowCount),
		},
		SizingRecommendation: MigrationSizingRecommendation{
			NumNodes:        ptrOf(float32(a.SizingRecommendation.NumNodes)),
			VcpuPerNode:     ptrOf(a.SizingRecommendation.VCPUPerNode),
			MemoryPerNodeGb: ptrOf(a.SizingRecommendation.MemoryPerNodeGB),
			ColocatedTables: strSlicePtr(a.SizingRecommendation.ColocatedTables),
			ShardedTables:   strSlicePtr(a.SizingRecommendation.ShardedTables),
			Summary:         ptrOf(a.SizingRecommendation.Summary),
		},
		Issues: make([]MigrationAssessmentIssue, 0, len(a.Issues)),
		Notes:  append([]string{}, a.Notes...),
	}
	for _, is := range a.Issues {
		out.Issues = append(out.Issues, MigrationAssessmentIssue{
			Category:    ptrOf(is.Category),
			Type:        ptrOf(is.Type),
			Name:        ptrOf(is.Name),
			Description: ptrOf(is.Description),
			Impact:      ptrOf(is.Impact),
			ObjectType:  ptrOf(is.ObjectType),
			ObjectName:  ptrOf(is.ObjectName),
			DocsLink:    ptrOf(is.DocsLink),
		})
	}
	return out
}

// ptrOf returns a pointer to v — the optional (omitempty) response fields are
// pointers, and the domain carries plain values.
func ptrOf[T any](v T) *T { return &v }

// strSlicePtr returns a pointer to a copy of s, or nil for an empty slice so an
// absent list is omitted rather than serialized as an empty array.
func strSlicePtr(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	c := append([]string(nil), s...)
	return &c
}
