package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// StepVoyagerAttachTarget is the task_step.kind that records a provisioned
// endpoint as a migration's target, the final step of create_migration_target.
const StepVoyagerAttachTarget = "attach_target_to_migration"

// voyagerTargetCredential is the fixed superuser role name a freshly deployed
// AMP compute exposes; the migrate job uses it to connect to the target.
//
// It is ALSO the target's password, which is a placeholder rather than a design
// — every other endpoint has required a caller-supplied one since #1272.
// VoyagerCreateMigrationTarget passes the verifier for it so the two agree.
// Replacing it with a real credential is yugabyte/meko#1473.
const voyagerTargetCredential = "cloud_admin"

// VoyagerAttachTargetArgs identifies the endpoint allocated by the plan, the
// name to give the target database (carried forward from the source so the
// migrate job imports into a like-named database), and the task holding the
// target provisioning claim (AttachTarget is fenced on it).
type VoyagerAttachTargetArgs struct {
	MigrationID        uuid.UUID `json:"migration_id"`
	TaskID             uuid.UUID `json:"task_id"`
	PoolID             uuid.UUID `json:"pool_id"`
	ClusterID          uuid.UUID `json:"cluster_id"`
	BranchID           uuid.UUID `json:"branch_id"`
	EndpointID         uuid.UUID `json:"endpoint_id"`
	TargetDatabaseName string    `json:"target_database_name"`
}

type voyagerAttachTargetStep struct {
	VoyagerAttachTargetArgs
	migrations migration.Store
	endpoints  VoyagerMigrationEndpointLookup
}

// NewVoyagerAttachTargetStep binds the target attachment args and
// collaborators into an executable step.
func NewVoyagerAttachTargetStep(args VoyagerAttachTargetArgs, deps Deps) Step {
	return &voyagerAttachTargetStep{
		VoyagerAttachTargetArgs: args,
		migrations:              deps.VoyagerMigrations,
		endpoints:               deps.VoyagerMigrationEndpoints,
	}
}

func buildVoyagerAttachTargetStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args VoyagerAttachTargetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode attach_target_to_migration args: %w", err)
	}
	return NewVoyagerAttachTargetStep(args, deps), nil
}

func (*voyagerAttachTargetStep) Kind() string { return StepVoyagerAttachTarget }

func (s *voyagerAttachTargetStep) Execute(ctx context.Context) StepResult {
	if s.endpoints == nil {
		return Fail(fmt.Errorf("attach_target_to_migration step requires an endpoint lookup, but none is configured"))
	}
	if s.migrations == nil {
		return Fail(fmt.Errorf("attach_target_to_migration step requires a migration store, but none is configured"))
	}

	address, err := s.endpoints.EndpointAddress(ctx, s.PoolID, s.ClusterID, s.BranchID, s.EndpointID)
	if err != nil {
		return Fail(fmt.Errorf("resolve migration target endpoint address: %w", err))
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return Fail(fmt.Errorf("migration target endpoint address %q must be explicit host:port: %w", address, err))
	}
	if host == "" {
		return Fail(fmt.Errorf("migration target endpoint address %q has an empty host", address))
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Fail(fmt.Errorf("migration target endpoint address %q has an invalid port", address))
	}

	target := migration.TargetConfig{
		PoolID:     s.PoolID,
		ClusterID:  s.ClusterID,
		BranchID:   s.BranchID,
		EndpointID: s.EndpointID,
		Host:       host,
		Port:       port,
		Username:   voyagerTargetCredential,
		Password:   voyagerTargetCredential,
		Database:   s.TargetDatabaseName,
	}
	if err := s.migrations.AttachTarget(ctx, s.MigrationID, s.TaskID, target); err != nil {
		return Fail(fmt.Errorf("attach migration target: %w", err))
	}

	// Deliberately omit Password: step results are exposed through the task API.
	return Ok(mustResult(map[string]any{
		"migration_id": s.MigrationID.String(),
		"pool_id":      s.PoolID.String(),
		"cluster_id":   s.ClusterID.String(),
		"branch_id":    s.BranchID.String(),
		"endpoint_id":  s.EndpointID.String(),
		"host":         host,
		"port":         port,
		"username":     voyagerTargetCredential,
		"database":     s.TargetDatabaseName,
	}))
}
